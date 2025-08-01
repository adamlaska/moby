package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration-cli/cli"
	"github.com/moby/moby/v2/testutil"
	"github.com/moby/moby/v2/testutil/request"
	"gotest.tools/v3/assert"
)

func (s *DockerAPISuite) TestLogsAPIWithStdout(c *testing.T) {
	out := cli.DockerCmd(c, "run", "-d", "-t", "busybox", "/bin/sh", "-c", "while true; do echo hello; sleep 1; done").Stdout()
	id := strings.TrimSpace(out)
	cli.WaitRun(c, id)

	type logOut struct {
		out string
		err error
	}

	chLog := make(chan logOut, 1)
	res, body, err := request.Get(testutil.GetContext(c), fmt.Sprintf("/containers/%s/logs?follow=1&stdout=1&timestamps=1", id))
	assert.NilError(c, err)
	assert.Equal(c, res.StatusCode, http.StatusOK)

	go func() {
		defer body.Close()
		out, err := bufio.NewReader(body).ReadString('\n')
		if err != nil {
			chLog <- logOut{"", err}
			return
		}
		chLog <- logOut{strings.TrimSpace(out), err}
	}()

	select {
	case l := <-chLog:
		assert.NilError(c, l.err)
		if !strings.HasSuffix(l.out, "hello") {
			c.Fatalf("expected log output to container 'hello', but it does not")
		}
	case <-time.After(30 * time.Second):
		c.Fatal("timeout waiting for logs to exit")
	}
}

func (s *DockerAPISuite) TestLogsAPINoStdoutNorStderr(c *testing.T) {
	const name = "logs_test"
	cli.DockerCmd(c, "run", "-d", "-t", "--name", name, "busybox", "/bin/sh")
	apiClient, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	_, err = apiClient.ContainerLogs(testutil.GetContext(c), name, container.LogsOptions{})
	assert.ErrorContains(c, err, "Bad parameters: you must choose at least one stream")
}

// Regression test for #12704
func (s *DockerAPISuite) TestLogsAPIFollowEmptyOutput(c *testing.T) {
	const name = "logs_test"
	t0 := time.Now()
	cli.DockerCmd(c, "run", "-d", "-t", "--name", name, "busybox", "sleep", "10")

	_, body, err := request.Get(testutil.GetContext(c), fmt.Sprintf("/containers/%s/logs?follow=1&stdout=1&stderr=1&tail=all", name))
	t1 := time.Now()
	assert.NilError(c, err)
	body.Close()
	elapsed := t1.Sub(t0).Seconds()
	if elapsed > 20.0 {
		c.Fatalf("HTTP response was not immediate (elapsed %.1fs)", elapsed)
	}
}

func (s *DockerAPISuite) TestLogsAPIContainerNotFound(c *testing.T) {
	name := "nonExistentContainer"
	resp, _, err := request.Get(testutil.GetContext(c), fmt.Sprintf("/containers/%s/logs?follow=1&stdout=1&stderr=1&tail=all", name))
	assert.NilError(c, err)
	assert.Equal(c, resp.StatusCode, http.StatusNotFound)
}

func (s *DockerAPISuite) TestLogsAPIUntilFutureFollow(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	const name = "logsuntilfuturefollow"
	cli.DockerCmd(c, "run", "-d", "--name", name, "busybox", "/bin/sh", "-c", "while true; do date +%s; sleep 1; done")
	cli.WaitRun(c, name)

	untilSecs := 5
	untilDur, err := time.ParseDuration(fmt.Sprintf("%ds", untilSecs))
	assert.NilError(c, err)
	until := daemonTime(c).Add(untilDur)

	apiClient, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		c.Fatal(err)
	}

	reader, err := apiClient.ContainerLogs(testutil.GetContext(c), name, container.LogsOptions{
		Until:      until.Format(time.RFC3339Nano),
		Follow:     true,
		ShowStdout: true,
		Timestamps: true,
	})
	assert.NilError(c, err)

	type logOut struct {
		out string
		err error
	}

	chLog := make(chan logOut)
	stop := make(chan struct{})
	defer close(stop)

	go func() {
		bufReader := bufio.NewReader(reader)
		defer reader.Close()
		for i := 0; i < untilSecs; i++ {
			out, _, err := bufReader.ReadLine()
			if err != nil {
				if err == io.EOF {
					return
				}
				select {
				case <-stop:
					return
				case chLog <- logOut{"", err}:
				}

				return
			}

			select {
			case <-stop:
				return
			case chLog <- logOut{strings.TrimSpace(string(out)), err}:
			}
		}
	}()

	for i := 0; i < untilSecs; i++ {
		select {
		case l := <-chLog:
			assert.NilError(c, l.err)
			i, err := strconv.ParseInt(strings.Split(l.out, " ")[1], 10, 64)
			assert.NilError(c, err)
			assert.Assert(c, time.Unix(i, 0).UnixNano() <= until.UnixNano())
		case <-time.After(20 * time.Second):
			c.Fatal("timeout waiting for logs to exit")
		}
	}
}

func (s *DockerAPISuite) TestLogsAPIUntil(c *testing.T) {
	const name = "logsuntil"
	cli.DockerCmd(c, "run", "--name", name, "busybox", "/bin/sh", "-c", "for i in $(seq 1 3); do echo log$i; sleep 1; done")

	apiClient, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		c.Fatal(err)
	}

	extractBody := func(t *testing.T, cfg container.LogsOptions) []string {
		reader, err := apiClient.ContainerLogs(testutil.GetContext(t), name, cfg)
		assert.NilError(t, err)

		actualStdout := new(bytes.Buffer)
		actualStderr := io.Discard
		_, err = stdcopy.StdCopy(actualStdout, actualStderr, reader)
		assert.NilError(t, err)

		return strings.Split(actualStdout.String(), "\n")
	}

	// Get timestamp of second log line
	allLogs := extractBody(c, container.LogsOptions{Timestamps: true, ShowStdout: true})
	assert.Assert(c, len(allLogs) >= 3)

	t, err := time.Parse(time.RFC3339Nano, strings.Split(allLogs[1], " ")[0])
	assert.NilError(c, err)
	until := t.Format(time.RFC3339Nano)

	// Get logs until the timestamp of second line, i.e. first two lines
	logs := extractBody(c, container.LogsOptions{Timestamps: true, ShowStdout: true, Until: until})

	// Ensure log lines after cut-off are excluded
	logsString := strings.Join(logs, "\n")
	assert.Assert(c, !strings.Contains(logsString, "log3"), "unexpected log message returned, until=%v", until)
}

func (s *DockerAPISuite) TestLogsAPIUntilDefaultValue(c *testing.T) {
	const name = "logsuntildefaultval"
	cli.DockerCmd(c, "run", "--name", name, "busybox", "/bin/sh", "-c", "for i in $(seq 1 3); do echo log$i; done")

	apiClient, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		c.Fatal(err)
	}

	extractBody := func(t *testing.T, cfg container.LogsOptions) []string {
		reader, err := apiClient.ContainerLogs(testutil.GetContext(t), name, cfg)
		assert.NilError(t, err)

		actualStdout := new(bytes.Buffer)
		actualStderr := io.Discard
		_, err = stdcopy.StdCopy(actualStdout, actualStderr, reader)
		assert.NilError(t, err)

		return strings.Split(actualStdout.String(), "\n")
	}

	// Get timestamp of second log line
	allLogs := extractBody(c, container.LogsOptions{Timestamps: true, ShowStdout: true})

	// Test with default value specified and parameter omitted
	defaultLogs := extractBody(c, container.LogsOptions{Timestamps: true, ShowStdout: true, Until: "0"})
	assert.DeepEqual(c, defaultLogs, allLogs)
}
