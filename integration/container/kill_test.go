package container

import (
	"runtime"
	"testing"

	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/testutil"
	"github.com/moby/moby/v2/testutil/request"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

func TestKillContainerInvalidSignal(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()
	id := container.Run(ctx, t, apiClient)

	err := apiClient.ContainerKill(ctx, id, "0")
	assert.ErrorContains(t, err, "Error response from daemon:")
	assert.ErrorContains(t, err, "nvalid signal: 0") // match "(I|i)nvalid" case-insensitive to allow testing against older daemons.
	poll.WaitOn(t, container.IsInState(ctx, apiClient, id, containertypes.StateRunning))

	err = apiClient.ContainerKill(ctx, id, "SIG42")
	assert.ErrorContains(t, err, "Error response from daemon:")
	assert.ErrorContains(t, err, "nvalid signal: SIG42") // match "(I|i)nvalid" case-insensitive to allow testing against older daemons.
	poll.WaitOn(t, container.IsInState(ctx, apiClient, id, containertypes.StateRunning))
}

func TestKillContainer(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	testCases := []struct {
		doc    string
		signal string
		status containertypes.ContainerState
		skipOs string
	}{
		{
			doc:    "no signal",
			signal: "",
			status: containertypes.StateExited,
			skipOs: "",
		},
		{
			doc:    "non killing signal",
			signal: "SIGWINCH",
			status: containertypes.StateRunning,
			skipOs: "windows",
		},
		{
			doc:    "killing signal",
			signal: "SIGTERM",
			status: containertypes.StateExited,
			skipOs: "",
		},
	}

	var pollOpts []poll.SettingOp
	if runtime.GOOS == "windows" {
		pollOpts = append(pollOpts, poll.WithTimeout(StopContainerWindowsPollTimeout))
	}

	for _, tc := range testCases {
		t.Run(tc.doc, func(t *testing.T) {
			skip.If(t, testEnv.DaemonInfo.OSType == tc.skipOs, "Windows does not support SIGWINCH")
			ctx := testutil.StartSpan(ctx, t)
			id := container.Run(ctx, t, apiClient)
			err := apiClient.ContainerKill(ctx, id, tc.signal)
			assert.NilError(t, err)

			poll.WaitOn(t, container.IsInState(ctx, apiClient, id, tc.status), pollOpts...)
		})
	}
}

func TestKillWithStopSignalAndRestartPolicies(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	testCases := []struct {
		doc        string
		stopsignal string
		status     string
	}{
		{
			doc:        "same-signal-disables-restart-policy",
			stopsignal: "TERM",
			status:     "exited",
		},
		{
			doc:        "different-signal-keep-restart-policy",
			stopsignal: "CONT",
			status:     "running",
		},
	}

	var pollOpts []poll.SettingOp
	if runtime.GOOS == "windows" {
		pollOpts = append(pollOpts, poll.WithTimeout(StopContainerWindowsPollTimeout))
	}

	for _, tc := range testCases {
		t.Run(tc.doc, func(t *testing.T) {
			ctx := testutil.StartSpan(ctx, t)
			id := container.Run(ctx, t, apiClient,
				container.WithRestartPolicy(containertypes.RestartPolicyAlways),
				func(c *container.TestContainerConfig) {
					c.Config.StopSignal = tc.stopsignal
				})
			err := apiClient.ContainerKill(ctx, id, "TERM")
			assert.NilError(t, err)

			poll.WaitOn(t, container.IsInState(ctx, apiClient, id, tc.status), pollOpts...)
		})
	}
}

func TestKillStoppedContainer(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()
	id := container.Create(ctx, t, apiClient)
	err := apiClient.ContainerKill(ctx, id, "SIGKILL")
	assert.ErrorContains(t, err, "")
	assert.ErrorContains(t, err, "is not running")
}

func TestKillDifferentUserContainer(t *testing.T) {
	// TODO Windows: Windows does not yet support -u (Feb 2016).
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "User containers (container.Config.User) are not yet supported on %q platform", testEnv.DaemonInfo.OSType)

	ctx := setupTest(t)
	apiClient := request.NewAPIClient(t)

	id := container.Run(ctx, t, apiClient, func(c *container.TestContainerConfig) {
		c.Config.User = "daemon"
	})

	err := apiClient.ContainerKill(ctx, id, "SIGKILL")
	assert.NilError(t, err)
	poll.WaitOn(t, container.IsInState(ctx, apiClient, id, containertypes.StateExited))
}

func TestInspectOomKilledTrue(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	skip.If(t, testEnv.DaemonInfo.CgroupDriver == "none")
	skip.If(t, !testEnv.DaemonInfo.MemoryLimit || !testEnv.DaemonInfo.SwapLimit)
	skip.If(t, testEnv.DaemonInfo.CgroupVersion == "2", "FIXME: flaky on cgroup v2 (https://github.com/moby/moby/issues/41929)")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	cID := container.Run(ctx, t, apiClient, container.WithCmd("sh", "-c", "x=a; while true; do x=$x$x$x$x; done"), func(c *container.TestContainerConfig) {
		c.HostConfig.Resources.Memory = 32 * 1024 * 1024
	})

	poll.WaitOn(t, container.IsInState(ctx, apiClient, cID, containertypes.StateExited))

	inspect, err := apiClient.ContainerInspect(ctx, cID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(true, inspect.State.OOMKilled))
}

func TestInspectOomKilledFalse(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows" || !testEnv.DaemonInfo.MemoryLimit || !testEnv.DaemonInfo.SwapLimit)

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	cID := container.Run(ctx, t, apiClient, container.WithCmd("sh", "-c", "echo hello world"))

	poll.WaitOn(t, container.IsInState(ctx, apiClient, cID, containertypes.StateExited))

	inspect, err := apiClient.ContainerInspect(ctx, cID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(false, inspect.State.OOMKilled))
}
