package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/libnetwork"
	"github.com/moby/moby/v2/daemon/libnetwork/diagnostic"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/overlay"
)

const (
	readyPath    = "http://%s:%d/ready"
	joinNetwork  = "http://%s:%d/joinnetwork?nid=%s"
	leaveNetwork = "http://%s:%d/leavenetwork?nid=%s"
	clusterPeers = "http://%s:%d/clusterpeers?json"
	networkPeers = "http://%s:%d/networkpeers?nid=%s&json"
	dumpTable    = "http://%s:%d/gettable?nid=%s&tname=%s&json"
	deleteEntry  = "http://%s:%d/deleteentry?nid=%s&tname=%s&key=%s&json"
)

func httpIsOk(body io.ReadCloser) {
	b, err := io.ReadAll(body)
	if err != nil {
		log.G(context.TODO()).Fatalf("Failed the body parse %s", err)
	}
	if !strings.Contains(string(b), "OK") {
		log.G(context.TODO()).Fatalf("Server not ready %s", b)
	}
	body.Close()
}

func main() {
	ipPtr := flag.String("ip", "127.0.0.1", "ip address")
	portPtr := flag.Int("port", 2000, "port")
	networkPtr := flag.String("net", "", "target network")
	tablePtr := flag.String("t", "", "table to process <sd/overlay>")
	remediatePtr := flag.Bool("r", false, "perform remediation deleting orphan entries")
	joinPtr := flag.Bool("a", false, "join/leave network")
	verbosePtr := flag.Bool("v", false, "verbose output")

	flag.Parse()

	if *verbosePtr {
		_ = log.SetLevel("debug")
	}

	if _, ok := os.LookupEnv("DIND_CLIENT"); !ok && *joinPtr {
		log.G(context.TODO()).Fatal("you are not using the client in docker in docker mode, the use of the -a flag can be disruptive, " +
			"please remove it (doc:https://github.com/docker/docker/libnetwork/blob/master/cmd/diagnostic/README.md)")
	}

	log.G(context.TODO()).Infof("Connecting to %s:%d checking ready", *ipPtr, *portPtr)
	resp, err := http.Get(fmt.Sprintf(readyPath, *ipPtr, *portPtr))
	if err != nil {
		log.G(context.TODO()).WithError(err).Fatalf("The connection failed")
	}
	httpIsOk(resp.Body)

	clusterPeers := fetchNodePeers(*ipPtr, *portPtr, "")
	var networkPeers map[string]string
	var joinedNetwork bool
	if *networkPtr != "" {
		if *joinPtr {
			log.G(context.TODO()).Infof("Joining the network:%q", *networkPtr)
			resp, err = http.Get(fmt.Sprintf(joinNetwork, *ipPtr, *portPtr, *networkPtr))
			if err != nil {
				log.G(context.TODO()).WithError(err).Fatalf("Failed joining the network")
			}
			httpIsOk(resp.Body)
			joinedNetwork = true
		}

		networkPeers = fetchNodePeers(*ipPtr, *portPtr, *networkPtr)
		if len(networkPeers) == 0 {
			log.G(context.TODO()).Warnf("There is no peer on network %q, check the network ID, and verify that is the non truncated version", *networkPtr)
		}
	}

	switch *tablePtr {
	case "sd":
		fetchTable(*ipPtr, *portPtr, *networkPtr, "endpoint_table", clusterPeers, networkPeers, *remediatePtr)
	case "overlay":
		fetchTable(*ipPtr, *portPtr, *networkPtr, "overlay_peer_table", clusterPeers, networkPeers, *remediatePtr)
	}

	if joinedNetwork {
		log.G(context.TODO()).Infof("Leaving the network:%q", *networkPtr)
		resp, err = http.Get(fmt.Sprintf(leaveNetwork, *ipPtr, *portPtr, *networkPtr))
		if err != nil {
			log.G(context.TODO()).WithError(err).Fatalf("Failed leaving the network")
		}
		httpIsOk(resp.Body)
	}
}

func fetchNodePeers(ip string, port int, network string) map[string]string {
	if network == "" {
		log.G(context.TODO()).Infof("Fetch cluster peers")
	} else {
		log.G(context.TODO()).Infof("Fetch peers network:%q", network)
	}

	var path string
	if network != "" {
		path = fmt.Sprintf(networkPeers, ip, port, network)
	} else {
		path = fmt.Sprintf(clusterPeers, ip, port)
	}

	resp, err := http.Get(path) //nolint:gosec // G107: Potential HTTP request made with variable url
	if err != nil {
		log.G(context.TODO()).WithError(err).Fatalf("Failed fetching path")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.G(context.TODO()).WithError(err).Fatalf("Failed the body parse")
	}

	output := diagnostic.HTTPResult{Details: &diagnostic.TablePeersResult{}}
	err = json.Unmarshal(body, &output)
	if err != nil {
		log.G(context.TODO()).WithError(err).Fatalf("Failed the json unmarshalling")
	}

	log.G(context.TODO()).Debugf("Parsing JSON response")
	result := make(map[string]string, output.Details.(*diagnostic.TablePeersResult).Length)
	for _, v := range output.Details.(*diagnostic.TablePeersResult).Elements {
		log.G(context.TODO()).Debugf("name:%s ip:%s", v.Name, v.IP)
		result[v.Name] = v.IP
	}
	return result
}

func fetchTable(ip string, port int, network, tableName string, clusterPeers, networkPeers map[string]string, remediate bool) {
	log.G(context.TODO()).Infof("Fetch %s table and check owners", tableName)
	resp, err := http.Get(fmt.Sprintf(dumpTable, ip, port, network, tableName))
	if err != nil {
		log.G(context.TODO()).WithError(err).Fatalf("Failed fetching endpoint table")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.G(context.TODO()).WithError(err).Fatalf("Failed the body parse")
	}

	output := diagnostic.HTTPResult{Details: &diagnostic.TableEndpointsResult{}}
	err = json.Unmarshal(body, &output)
	if err != nil {
		log.G(context.TODO()).WithError(err).Fatalf("Failed the json unmarshalling")
	}

	log.G(context.TODO()).Debug("Parsing data structures")
	var orphanKeys []string
	for _, v := range output.Details.(*diagnostic.TableEndpointsResult).Elements {
		decoded, err := base64.StdEncoding.DecodeString(v.Value)
		if err != nil {
			log.G(context.TODO()).WithError(err).Errorf("Failed decoding entry")
			continue
		}
		switch tableName {
		case "endpoint_table":
			var elem libnetwork.EndpointRecord
			_ = elem.Unmarshal(decoded)
			log.G(context.TODO()).Debugf("key:%s value:%+v owner:%s", v.Key, elem, v.Owner)
		case "overlay_peer_table":
			var elem overlay.PeerRecord
			_ = elem.Unmarshal(decoded)
			log.G(context.TODO()).Debugf("key:%s value:%+v owner:%s", v.Key, elem, v.Owner)
		}

		if _, ok := networkPeers[v.Owner]; !ok {
			log.G(context.TODO()).Warnf("The element with key:%s does not belong to any node on this network", v.Key)
			orphanKeys = append(orphanKeys, v.Key)
		}
		if _, ok := clusterPeers[v.Owner]; !ok {
			log.G(context.TODO()).Warnf("The element with key:%s does not belong to any node on this cluster", v.Key)
		}
	}

	if len(orphanKeys) > 0 && remediate {
		log.G(context.TODO()).Warnf("The following keys:%v results as orphan, do you want to proceed with the deletion (this operation is irreversible)? [Yes/No]", orphanKeys)
		reader := bufio.NewReader(os.Stdin)
		text, _ := reader.ReadString('\n')
		text = strings.ReplaceAll(text, "\n", "")
		if text == "Yes" {
			for _, k := range orphanKeys {
				r, err := http.Get(fmt.Sprintf(deleteEntry, ip, port, network, tableName, k))
				if err != nil {
					log.G(context.TODO()).WithError(err).Errorf("Failed deleting entry k:%s", k)
					break
				}
				_ = r.Body.Close()
			}
		} else {
			log.G(context.TODO()).Infof("Deletion skipped")
		}
	}
}
