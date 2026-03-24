package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
)

// RunInContainer clones a git repo on the host, mounts it into an Ubuntu
// container, runs the provided commands inside it, then cleans up.
func RunInContainer(ctx context.Context, repoURL string, commands []string) error {
	// 1. Clone the repo to a temp dir on the host
	// Use home dir instead of /tmp — Docker Desktop on macOS only bind-mounts
	// directories under /Users by default; /tmp (/private/tmp) is blocked.
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("get home dir: %w", err)
	}
	repoDir, err := os.MkdirTemp(homeDir, "repo-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(repoDir)

	fmt.Printf("Cloning %s into %s\n", repoURL, repoDir)
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth=1", repoURL, repoDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}

	// Make the path absolute (required for Docker bind mounts)
	absRepoDir, err := filepath.Abs(repoDir)
	if err != nil {
		return fmt.Errorf("resolve abs path: %w", err)
	}

	// 2. Connect to Docker
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer cli.Close()

	// 3. Pull the Ubuntu image if not already present
	// const dockerImage = "ubuntu:24.04"
	const dockerImage = "golang:1.26-alpine"
	fmt.Printf("Pulling image %s\n", dockerImage)
	reader, err := cli.ImagePull(ctx, dockerImage, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("image pull: %w", err)
	}
	io.Copy(io.Discard, reader) // drain so the pull completes
	reader.Close()

	// 4. Create the container with the repo bind-mounted at /workspace
	// resp, err := cli.ContainerCreate(ctx,
	// &container.Config{
	// Image:      dockerImage,
	// WorkingDir: "/workspace",
	// Keep the container alive so we can exec into it
	// Cmd: []string{"sleep", "infinity"},
	// },
	// &container.HostConfig{
	// Mounts: []mount.Mount{
	// {
	// Type:   mount.TypeBind,
	// Source: absRepoDir,
	// Target: "/workspace",
	// },
	// },
	// },
	// nil, nil, "",
	// )
	// import (
	// "github.com/docker/go-connections/nat"
	// )

	resp, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:      dockerImage,
			WorkingDir: "/workspace",
			Cmd:        []string{"sleep", "infinity"},
			// Add this — ports the container listens on
			ExposedPorts: nat.PortSet{
				"8080/tcp": struct{}{},
				"5432/tcp": struct{}{},
			},
		},
		&container.HostConfig{
			Mounts: []mount.Mount{
				{
					Type:   mount.TypeBind,
					Source: absRepoDir,
					Target: "/workspace",
				},
			},
			// Add this — maps host port → container port
			PortBindings: nat.PortMap{
				"8080/tcp": []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "8080"}},
				"5432/tcp": []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "5432"}},
			},
		},
		nil, nil, "",
	)
	if err != nil {
		return fmt.Errorf("container create: %w", err)
	}
	containerID := resp.ID

	// Ensure cleanup regardless of what happens next
	defer func() {
		fmt.Println("Removing container...")
		cli.ContainerRemove(context.Background(), containerID, container.RemoveOptions{Force: true})
	}()

	// 5. Start the container
	if err := cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return fmt.Errorf("container start: %w", err)
	}
	fmt.Printf("Container %s started\n", containerID[:12])

	// 6. Run each command via exec
	for _, command := range commands {
		if err := execInContainer(ctx, cli, containerID, command); err != nil {
			return fmt.Errorf("exec %q: %w", command, err)
		}
	}

	return nil
}

// execInContainer runs a single shell command inside the container and streams
// its output to stdout/stderr.
func execInContainer(ctx context.Context, cli *client.Client, containerID, command string) error {
	fmt.Printf("\n--- Running: %s ---\n", command)

	execID, err := cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          []string{"/bin/sh", "-c", command},
		AttachStdout: true,
		AttachStderr: true,
		WorkingDir:   "/workspace",
	})
	if err != nil {
		return fmt.Errorf("exec create: %w", err)
	}

	attach, err := cli.ContainerExecAttach(ctx, execID.ID, container.ExecStartOptions{})
	if err != nil {
		return fmt.Errorf("exec attach: %w", err)
	}
	defer attach.Close()

	// Stream output — demultiplex Docker's combined stdout/stderr stream
	if _, err := stdcopy.StdCopy(os.Stdout, os.Stderr, attach.Reader); err != nil && err != io.EOF {
		return fmt.Errorf("stream output: %w", err)
	}

	// Wait for the exec to fully finish before reading the exit code
	var inspect container.ExecInspect
	for {
		inspect, err = cli.ContainerExecInspect(ctx, execID.ID)
		if err != nil {
			return fmt.Errorf("exec inspect: %w", err)
		}
		if !inspect.Running {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if inspect.ExitCode != 0 {
		return fmt.Errorf("exited with code %d", inspect.ExitCode)
	}

	return nil
}

func main() {
	err := RunInContainer(
		context.Background(),
		"https://github.com/sdnxshu/basic-go-gin-app.git",
		[]string{
			// "apt-get update -qq && apt-get install -y -qq golang-go",
			// "go build ./...",
			// "go test ./...",
			"go build -o app",
			"./app",
		},
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
