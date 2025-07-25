package container

import (
	"context"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/pkg/errors"
	"gotest.tools/v3/poll"
)

// RunningStateFlagIs polls for the container's Running state flag to be equal to running.
func RunningStateFlagIs(ctx context.Context, apiClient client.APIClient, containerID string, running bool) func(log poll.LogT) poll.Result {
	return func(log poll.LogT) poll.Result {
		inspect, err := apiClient.ContainerInspect(ctx, containerID)

		switch {
		case err != nil:
			return poll.Error(err)
		case inspect.State.Running == running:
			return poll.Success()
		default:
			return poll.Continue("waiting for container to be %s", map[bool]string{true: "running", false: "stopped"}[running])
		}
	}
}

// IsStopped verifies the container is in stopped state.
func IsStopped(ctx context.Context, apiClient client.APIClient, containerID string) func(log poll.LogT) poll.Result {
	return RunningStateFlagIs(ctx, apiClient, containerID, false)
}

// IsInState verifies the container is in one of the specified state, e.g., "running", "exited", etc.
func IsInState(ctx context.Context, apiClient client.APIClient, containerID string, state ...container.ContainerState) func(log poll.LogT) poll.Result {
	return func(log poll.LogT) poll.Result {
		inspect, err := apiClient.ContainerInspect(ctx, containerID)
		if err != nil {
			return poll.Error(err)
		}
		for _, v := range state {
			if inspect.State.Status == v {
				return poll.Success()
			}
		}
		if len(state) == 1 {
			return poll.Continue("waiting for container State.Status to be '%s', currently '%s'", state[0], inspect.State.Status)
		} else {
			return poll.Continue("waiting for container State.Status to be one of (%s), currently '%s'", strings.Join(state, ", "), inspect.State.Status)
		}
	}
}

// IsSuccessful verifies state.Status == "exited" && state.ExitCode == 0
func IsSuccessful(ctx context.Context, apiClient client.APIClient, containerID string) func(log poll.LogT) poll.Result {
	return func(log poll.LogT) poll.Result {
		inspect, err := apiClient.ContainerInspect(ctx, containerID)
		if err != nil {
			return poll.Error(err)
		}
		if inspect.State.Status == container.StateExited {
			if inspect.State.ExitCode == 0 {
				return poll.Success()
			}
			return poll.Error(errors.Errorf("expected exit code 0, got %d", inspect.State.ExitCode))
		}
		return poll.Continue("waiting for container to be %q, currently %s", container.StateExited, inspect.State.Status)
	}
}

// IsRemoved verifies the container has been removed
func IsRemoved(ctx context.Context, apiClient client.APIClient, containerID string) func(log poll.LogT) poll.Result {
	return func(log poll.LogT) poll.Result {
		inspect, err := apiClient.ContainerInspect(ctx, containerID)
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				return poll.Success()
			}
			return poll.Error(err)
		}
		return poll.Continue("waiting for container to be removed, currently %s", inspect.State.Status)
	}
}
