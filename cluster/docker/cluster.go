package docker

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/rand"

	"os"
	"strconv"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/guseggert/clustertest/agent"
	clusteriface "github.com/guseggert/clustertest/cluster"
	"github.com/guseggert/clustertest/internal/files"
	"github.com/guseggert/clustertest/internal/net"
)

const chars = "abcefghijklmnopqrstuvwxyz0123456789"

func init() {
	rand.Seed(time.Now().UnixNano())
}

func randString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

// Cluster is a local Cluster that runs nodes as Docker containers.
// The underlying host must have a Docker daemon running.
// This supports standard environment variables for configuring the Docker client (DOCKER_HOST etc.).
type Cluster struct {
	Cert            *agent.Certs
	BaseImage       string
	ContainerPrefix string
	DockerClient    *client.Client
	Nodes           []*node

	imagePulled bool
}

type Option func(c *Cluster)

func NewCluster(baseImage string, opts ...Option) (*Cluster, error) {
	dockerClient, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("building Docker client: %w", err)
	}
	cert, err := agent.GenerateCert()
	if err != nil {
		return nil, fmt.Errorf("generating TLS cert: %w", err)
	}
	return &Cluster{
		Cert:            cert,
		BaseImage:       baseImage,
		DockerClient:    dockerClient,
		ContainerPrefix: randString(6),
	}, nil
}

func (c *Cluster) ensureImagePulled(ctx context.Context) error {
	if c.imagePulled {
		return nil
	}
	out, err := c.DockerClient.ImagePull(ctx, c.BaseImage, types.ImagePullOptions{})
	if err != nil {
		if out != nil {
			out.Close()
		}
		return err
	}
	defer out.Close()
	_, err = io.Copy(io.Discard, out)
	if err != nil {
		return fmt.Errorf("reading Docker pull response: %w", err)
	}
	c.imagePulled = true
	return nil
}

func (c *Cluster) NewNodes(ctx context.Context, n int) (clusteriface.Nodes, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("getting wd: %w", err)
	}
	nodeAgentBin := files.FindUp("nodeagent", wd)
	if nodeAgentBin == "" {
		return nil, errors.New("unable to find nodeagent bin")
	}

	err = c.ensureImagePulled(ctx)
	if err != nil {
		return nil, fmt.Errorf("pulling image: %w", err)
	}

	startID := len(c.Nodes)
	var newNodes []clusteriface.Node
	for i := 0; i < n; i++ {
		id := startID + i
		containerName := fmt.Sprintf("clustertest-%s-%d", c.ContainerPrefix, id)

		hostPort, err := net.GetEphemeralTCPPort()
		if err != nil {
			return nil, fmt.Errorf("acquiring ephemeral port: %w", err)
		}

		caCertPEMEncoded := base64.StdEncoding.EncodeToString(c.Cert.CA.CertPEMBytes)
		certPEMEncoded := base64.StdEncoding.EncodeToString(c.Cert.Server.CertPEMBytes)
		keyPEMEncoded := base64.StdEncoding.EncodeToString(c.Cert.Server.KeyPEMBytes)

		createResp, err := c.DockerClient.ContainerCreate(
			ctx,
			&container.Config{
				Image: c.BaseImage,
				Entrypoint: []string{"/nodeagent",
					"--ca-cert-pem", caCertPEMEncoded,
					"--cert-pem", certPEMEncoded,
					"--key-pem", keyPEMEncoded,
					"--on-heartbeat-failure", "exit",
					"--listen-addr", "0.0.0.0:8080",
				},
				ExposedPorts: nat.PortSet{"8080": struct{}{}},
			},
			&container.HostConfig{
				Binds:        []string{fmt.Sprintf("%s:/nodeagent", nodeAgentBin)},
				PortBindings: nat.PortMap{"8080": []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: strconv.Itoa(hostPort)}}},
			},
			nil,
			nil,
			containerName,
		)
		if err != nil {
			return nil, fmt.Errorf("creating Docker container: %w", err)
		}

		containerID := createResp.ID

		err = c.DockerClient.ContainerStart(ctx, containerID, types.ContainerStartOptions{})
		if err != nil {
			return nil, fmt.Errorf("starting container %q: %w", containerID, err)
		}

		agentClient, err := agent.NewClient(c.Cert, "127.0.0.1", hostPort)
		if err != nil {
			return nil, fmt.Errorf("building nodeagent client: %w", err)
		}

		node := &node{
			ID:            id,
			ContainerName: containerName,
			ContainerID:   createResp.ID,
			HostPort:      hostPort,
			Env:           map[string]string{},
			agentClient:   agentClient,
			dockerClient:  c.DockerClient,
		}

		newNodes = append(newNodes, node)
		c.Nodes = append(c.Nodes, node)
	}

	for _, n := range newNodes {
		n.(*node).agentClient.WaitForServer(ctx)
	}
	return newNodes, nil
}

func (c *Cluster) Cleanup(ctx context.Context) error {
	return nil
}