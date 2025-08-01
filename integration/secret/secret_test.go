package secret

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/filters"
	swarmtypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/swarm"
	"github.com/moby/moby/v2/testutil"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

func TestSecretInspect(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")

	ctx := setupTest(t)
	d := swarm.NewSwarm(ctx, t, testEnv)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()

	testName := t.Name()
	secretID := createSecret(ctx, t, c, testName, []byte("TESTINGDATA"), nil)

	insp, body, err := c.SecretInspectWithRaw(ctx, secretID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(insp.Spec.Name, testName))

	var secret swarmtypes.Secret
	err = json.Unmarshal(body, &secret)
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(secret, insp))
}

func TestSecretList(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")

	ctx := setupTest(t)
	d := swarm.NewSwarm(ctx, t, testEnv)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()

	configs, err := c.SecretList(ctx, swarmtypes.SecretListOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(configs), 0))

	testName0 := "test0_" + t.Name()
	testName1 := "test1_" + t.Name()
	testNames := []string{testName0, testName1}
	sort.Strings(testNames)

	// create secret test0
	createSecret(ctx, t, c, testName0, []byte("TESTINGDATA0"), map[string]string{"type": "test"})

	// create secret test1
	secret1ID := createSecret(ctx, t, c, testName1, []byte("TESTINGDATA1"), map[string]string{"type": "production"})

	// test by `secret ls`
	entries, err := c.SecretList(ctx, swarmtypes.SecretListOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(secretNamesFromList(entries), testNames))

	testCases := []struct {
		filters  filters.Args
		expected []string
	}{
		// test filter by name `secret ls --filter name=xxx`
		{
			filters:  filters.NewArgs(filters.Arg("name", testName0)),
			expected: []string{testName0},
		},
		// test filter by id `secret ls --filter id=xxx`
		{
			filters:  filters.NewArgs(filters.Arg("id", secret1ID)),
			expected: []string{testName1},
		},
		// test filter by label `secret ls --filter label=xxx`
		{
			filters:  filters.NewArgs(filters.Arg("label", "type")),
			expected: testNames,
		},
		{
			filters:  filters.NewArgs(filters.Arg("label", "type=test")),
			expected: []string{testName0},
		},
		{
			filters:  filters.NewArgs(filters.Arg("label", "type=production")),
			expected: []string{testName1},
		},
	}
	for _, tc := range testCases {
		entries, err = c.SecretList(ctx, swarmtypes.SecretListOptions{
			Filters: tc.filters,
		})
		assert.NilError(t, err)
		assert.Check(t, is.DeepEqual(secretNamesFromList(entries), tc.expected))
	}
}

func createSecret(ctx context.Context, t *testing.T, client client.APIClient, name string, data []byte, labels map[string]string) string {
	secret, err := client.SecretCreate(ctx, swarmtypes.SecretSpec{
		Annotations: swarmtypes.Annotations{
			Name:   name,
			Labels: labels,
		},
		Data: data,
	})
	assert.NilError(t, err)
	assert.Check(t, secret.ID != "")
	return secret.ID
}

func TestSecretsCreateAndDelete(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")

	ctx := setupTest(t)
	d := swarm.NewSwarm(ctx, t, testEnv)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()

	testName := "test_secret_" + t.Name()
	secretID := createSecret(ctx, t, c, testName, []byte("TESTINGDATA"), nil)

	// create an already existing secret, daemon should return a status code of 409
	_, err := c.SecretCreate(ctx, swarmtypes.SecretSpec{
		Annotations: swarmtypes.Annotations{
			Name: testName,
		},
		Data: []byte("TESTINGDATA"),
	})
	assert.Check(t, cerrdefs.IsConflict(err))
	assert.Check(t, is.ErrorContains(err, testName))

	err = c.SecretRemove(ctx, secretID)
	assert.NilError(t, err)

	_, _, err = c.SecretInspectWithRaw(ctx, secretID)
	assert.Check(t, cerrdefs.IsNotFound(err))
	assert.Check(t, is.ErrorContains(err, secretID))

	err = c.SecretRemove(ctx, "non-existing")
	assert.Check(t, cerrdefs.IsNotFound(err))
	assert.Check(t, is.ErrorContains(err, "non-existing"))

	testName = "test_secret_with_labels_" + t.Name()
	secretID = createSecret(ctx, t, c, testName, []byte("TESTINGDATA"), map[string]string{
		"key1": "value1",
		"key2": "value2",
	})

	insp, _, err := c.SecretInspectWithRaw(ctx, secretID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(insp.Spec.Name, testName))
	assert.Check(t, is.Equal(len(insp.Spec.Labels), 2))
	assert.Check(t, is.Equal(insp.Spec.Labels["key1"], "value1"))
	assert.Check(t, is.Equal(insp.Spec.Labels["key2"], "value2"))
}

func TestSecretsUpdate(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")

	ctx := setupTest(t)
	d := swarm.NewSwarm(ctx, t, testEnv)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()

	testName := "test_secret_" + t.Name()
	secretID := createSecret(ctx, t, c, testName, []byte("TESTINGDATA"), nil)

	insp, _, err := c.SecretInspectWithRaw(ctx, secretID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(insp.ID, secretID))

	// test UpdateSecret with full ID
	insp.Spec.Labels = map[string]string{"test": "test1"}
	err = c.SecretUpdate(ctx, secretID, insp.Version, insp.Spec)
	assert.NilError(t, err)

	insp, _, err = c.SecretInspectWithRaw(ctx, secretID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(insp.Spec.Labels["test"], "test1"))

	// test UpdateSecret with full name
	insp.Spec.Labels = map[string]string{"test": "test2"}
	err = c.SecretUpdate(ctx, testName, insp.Version, insp.Spec)
	assert.NilError(t, err)

	insp, _, err = c.SecretInspectWithRaw(ctx, secretID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(insp.Spec.Labels["test"], "test2"))

	// test UpdateSecret with prefix ID
	insp.Spec.Labels = map[string]string{"test": "test3"}
	err = c.SecretUpdate(ctx, secretID[:1], insp.Version, insp.Spec)
	assert.NilError(t, err)

	insp, _, err = c.SecretInspectWithRaw(ctx, secretID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(insp.Spec.Labels["test"], "test3"))

	// test UpdateSecret in updating Data which is not supported in daemon
	// this test will produce an error in func UpdateSecret
	insp.Spec.Data = []byte("TESTINGDATA2")
	err = c.SecretUpdate(ctx, secretID, insp.Version, insp.Spec)
	assert.Check(t, cerrdefs.IsInvalidArgument(err))
	assert.Check(t, is.ErrorContains(err, "only updates to Labels are allowed"))
}

func TestTemplatedSecret(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")

	ctx := testutil.StartSpan(baseContext, t)

	d := swarm.NewSwarm(ctx, t, testEnv)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()

	referencedSecretName := "referencedsecret_" + t.Name()
	referencedSecretSpec := swarmtypes.SecretSpec{
		Annotations: swarmtypes.Annotations{
			Name: referencedSecretName,
		},
		Data: []byte("this is a secret"),
	}
	referencedSecret, err := c.SecretCreate(ctx, referencedSecretSpec)
	assert.Check(t, err)

	referencedConfigName := "referencedconfig_" + t.Name()
	referencedConfigSpec := swarmtypes.ConfigSpec{
		Annotations: swarmtypes.Annotations{
			Name: referencedConfigName,
		},
		Data: []byte("this is a config"),
	}
	referencedConfig, err := c.ConfigCreate(ctx, referencedConfigSpec)
	assert.Check(t, err)

	templatedSecretName := "templated_secret_" + t.Name()
	secretSpec := swarmtypes.SecretSpec{
		Annotations: swarmtypes.Annotations{
			Name: templatedSecretName,
		},
		Templating: &swarmtypes.Driver{
			Name: "golang",
		},
		Data: []byte("SERVICE_NAME={{.Service.Name}}\n" +
			"{{secret \"referencedsecrettarget\"}}\n" +
			"{{config \"referencedconfigtarget\"}}\n"),
	}

	templatedSecret, err := c.SecretCreate(ctx, secretSpec)
	assert.Check(t, err)

	const serviceName = "svc_templated_secret"
	serviceID := swarm.CreateService(ctx, t, d,
		swarm.ServiceWithSecret(
			&swarmtypes.SecretReference{
				File: &swarmtypes.SecretReferenceFileTarget{
					Name: "templated_secret",
					UID:  "0",
					GID:  "0",
					Mode: 0o600,
				},
				SecretID:   templatedSecret.ID,
				SecretName: templatedSecretName,
			},
		),
		swarm.ServiceWithConfig(
			&swarmtypes.ConfigReference{
				File: &swarmtypes.ConfigReferenceFileTarget{
					Name: "referencedconfigtarget",
					UID:  "0",
					GID:  "0",
					Mode: 0o600,
				},
				ConfigID:   referencedConfig.ID,
				ConfigName: referencedConfigName,
			},
		),
		swarm.ServiceWithSecret(
			&swarmtypes.SecretReference{
				File: &swarmtypes.SecretReferenceFileTarget{
					Name: "referencedsecrettarget",
					UID:  "0",
					GID:  "0",
					Mode: 0o600,
				},
				SecretID:   referencedSecret.ID,
				SecretName: referencedSecretName,
			},
		),
		swarm.ServiceWithName(serviceName),
	)

	poll.WaitOn(t, swarm.RunningTasksCount(ctx, c, serviceID, 1), swarm.ServicePoll, poll.WithTimeout(1*time.Minute))

	tasks := swarm.GetRunningTasks(ctx, t, c, serviceID)
	assert.Assert(t, len(tasks) > 0, "no running tasks found for service %s", serviceID)

	resp := swarm.ExecTask(ctx, t, d, tasks[0], container.ExecOptions{
		Cmd:          []string{"/bin/cat", "/run/secrets/templated_secret"},
		AttachStdout: true,
		AttachStderr: true,
	})

	const expect = "SERVICE_NAME=" + serviceName + "\nthis is a secret\nthis is a config\n"
	var outBuf, errBuf bytes.Buffer
	_, err = stdcopy.StdCopy(&outBuf, &errBuf, resp.Reader)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(outBuf.String(), expect))
	assert.Check(t, is.Equal(errBuf.String(), ""))

	outBuf.Reset()
	errBuf.Reset()
	resp = swarm.ExecTask(ctx, t, d, tasks[0], container.ExecOptions{
		Cmd:          []string{"mount"},
		AttachStdout: true,
		AttachStderr: true,
	})

	_, err = stdcopy.StdCopy(&outBuf, &errBuf, resp.Reader)
	assert.NilError(t, err)
	assert.Check(t, is.Contains(outBuf.String(), "tmpfs on /run/secrets/templated_secret type tmpfs"), "expected to be mounted as tmpfs")
	assert.Check(t, is.Equal(errBuf.String(), ""))
}

// Test case for 28884
func TestSecretCreateResolve(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")

	ctx := setupTest(t)
	d := swarm.NewSwarm(ctx, t, testEnv)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()

	testName := "test_secret_" + t.Name()
	secretID := createSecret(ctx, t, c, testName, []byte("foo"), nil)

	fakeName := secretID
	fakeID := createSecret(ctx, t, c, fakeName, []byte("fake foo"), nil)

	entries, err := c.SecretList(ctx, swarmtypes.SecretListOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.Contains(secretNamesFromList(entries), testName))
	assert.Check(t, is.Contains(secretNamesFromList(entries), fakeName))

	err = c.SecretRemove(ctx, secretID)
	assert.NilError(t, err)

	// Fake one will remain
	entries, err = c.SecretList(ctx, swarmtypes.SecretListOptions{})
	assert.NilError(t, err)
	assert.Assert(t, is.DeepEqual(secretNamesFromList(entries), []string{fakeName}))

	// Remove based on name prefix of the fake one should not work
	// as search is only done based on:
	// - Full ID
	// - Full Name
	// - Partial ID (prefix)
	err = c.SecretRemove(ctx, fakeName[:5])
	assert.Assert(t, err != nil)
	entries, err = c.SecretList(ctx, swarmtypes.SecretListOptions{})
	assert.NilError(t, err)
	assert.Assert(t, is.DeepEqual(secretNamesFromList(entries), []string{fakeName}))

	// Remove based on ID prefix of the fake one should succeed
	err = c.SecretRemove(ctx, fakeID[:5])
	assert.NilError(t, err)
	entries, err = c.SecretList(ctx, swarmtypes.SecretListOptions{})
	assert.NilError(t, err)
	assert.Assert(t, is.Equal(0, len(entries)))
}

func secretNamesFromList(entries []swarmtypes.Secret) []string {
	var values []string
	for _, entry := range entries {
		values = append(values, entry.Spec.Name)
	}
	sort.Strings(values)
	return values
}
