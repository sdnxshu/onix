package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

// RunInContainer clones a git repo on the host, mounts it into an Ubuntu
// container, runs the provided commands inside it, then cleans up.
func RunInContainer(ctx context.Context, repoURL string, commands []string) error {
	// 1. Clone the repo to a temp dir on the host
	repoDir, err := os.MkdirTemp("", "repo-*")
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
	const dockerImage = "oven/bun:alpine"
	fmt.Printf("Pulling image %s\n", dockerImage)
	reader, err := cli.ImagePull(ctx, dockerImage, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("image pull: %w", err)
	}
	io.Copy(io.Discard, reader) // drain so the pull completes
	reader.Close()

	// 4. Create the container with the repo bind-mounted at /workspace
	resp, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:      dockerImage,
			WorkingDir: "/workspace",
			Cmd:        []string{"sleep", "infinity"},
			// Add this — ports the container listens on
			ExposedPorts: nat.PortSet{
				// "8080/tcp": struct{}{},
				// "5432/tcp": struct{}{},
				"3000/tcp": struct{}{},
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
				// "8080/tcp": []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "8080"}},
				// "5432/tcp": []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "5432"}},
				"3000/tcp": []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "3000"}},
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

	// Stream output — Docker multiplexes stdout/stderr on one connection
	if _, err := io.Copy(os.Stdout, attach.Reader); err != nil && err != io.EOF {
		return fmt.Errorf("stream output: %w", err)
	}

	// Check the exit code
	inspect, err := cli.ContainerExecInspect(ctx, execID.ID)
	if err != nil {
		return fmt.Errorf("exec inspect: %w", err)
	}
	if inspect.ExitCode != 0 {
		return fmt.Errorf("exited with code %d", inspect.ExitCode)
	}

	return nil
}

func main() {
	err := RunInContainer(
		context.Background(),
		"https://github.com/sdnxshu/jennings-test.git",
		[]string{
			// "go build -o app .",
			// "./app",
			"bun install",
			"bun run dev",
		},
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
