package agent

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"

	"github.com/guseggert/clustertest/cluster"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostFile(t *testing.T) {
	cert, err := GenerateCert()
	require.NoError(t, err)
	agent, err := NewNodeAgent(
		cert.CA.CertPEMBytes,
		cert.Server.CertPEMBytes,
		cert.Server.KeyPEMBytes,
		WithListenAddr("127.0.0.1:9998"),
	)
	require.NoError(t, err)

	go agent.Run()
	defer func() {
		require.NoError(t, agent.Stop())
	}()

	client, err := NewClient(cert, "127.0.0.1", 9998)
	require.NoError(t, err)

	err = client.WaitForServer(context.Background())
	require.NoError(t, err)

	req := cluster.SendFileRequest{FilePath: "/tmp/hello", Contents: bytes.NewBuffer([]byte("y helo thar"))}
	err = client.SendFile(context.Background(), req)
	require.NoError(t, err)
}

func TestConnect(t *testing.T) {
	ctx := context.Background()

	cert, err := GenerateCert()
	require.NoError(t, err)

	agent, err := NewNodeAgent(
		cert.CA.CertPEMBytes,
		cert.Server.CertPEMBytes,
		cert.Server.KeyPEMBytes,
		WithListenAddr("127.0.0.1:9998"),
	)
	require.NoError(t, err)

	go agent.Run()
	defer func() {
		require.NoError(t, agent.Stop())
	}()

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))
	}))
	t.Cleanup(s.Close)

	u, err := url.Parse(s.URL)
	require.NoError(t, err)
	addrPort, err := netip.ParseAddrPort(u.Host)
	require.NoError(t, err)

	client, err := NewClient(cert, "127.0.0.1", 9998)
	require.NoError(t, err)

	err = client.WaitForServer(ctx)
	require.NoError(t, err)

	conn, err := client.Dial("tcp", addrPort.String())
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodGet, "/", nil)
	require.NoError(t, err)

	err = req.Write(conn)
	require.NoError(t, err)

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, "hello", string(b))
}

func TestCommand(t *testing.T) {
	ctx := context.Background()

	cert, err := GenerateCert()
	require.NoError(t, err)

	agent, err := NewNodeAgent(
		cert.CA.CertPEMBytes,
		cert.Server.CertPEMBytes,
		cert.Server.KeyPEMBytes,
		WithListenAddr("127.0.0.1:9998"),
	)
	require.NoError(t, err)

	go agent.Run()
	defer func() {
		require.NoError(t, agent.Stop())
	}()

	client, err := NewClient(cert, "127.0.0.1", 9998)
	require.NoError(t, err)

	err = client.WaitForServer(ctx)
	require.NoError(t, err)

	cases := []struct {
		name      string
		cmd       string
		args      []string
		stdin     string
		expStdout string
		expStderr string
	}{
		{
			name:      "happy case",
			cmd:       "echo",
			args:      []string{"hello"},
			expStdout: "hello\n",
		},
		{
			name: "happy case, no stdout reader",
			cmd:  "echo",
			args: []string{"hello"},
		},
		{
			name:      "happy case with stdout and stderr readers",
			cmd:       "sh",
			args:      []string{"-c", "printf foo; printf bar 1>&2"},
			expStdout: "foo",
			expStderr: "bar",
		},
		{
			name: "happy case with no stderr and stdout readers",
			cmd:  "sh",
			args: []string{"-c", "printf foo; printf bar 1>&2"},
		},
		{
			name:      "stdin to stdout",
			cmd:       "sh",
			args:      []string{"-c", "read line; echo $line bar"},
			stdin:     "foo",
			expStdout: "foo bar\n",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runReq := cluster.RunRequest{
				Command: c.cmd,
				Args:    c.args,
			}

			var stdoutBuf bytes.Buffer
			if c.expStdout != "" {
				runReq.Stdout = &noopWriteCloser{Writer: &stdoutBuf}
			}
			var stderrBuf bytes.Buffer
			if c.expStderr != "" {
				runReq.Stderr = &noopWriteCloser{Writer: &stderrBuf}
			}

			if c.stdin != "" {
				runReq.Stdin = bytes.NewReader([]byte(c.stdin))
			}

			wait, err := client.Run(ctx, runReq)
			require.NoError(t, err)

			exitCode, err := wait(ctx)
			require.NoError(t, err)

			assert.Equal(t, 0, exitCode)

			if c.expStdout != "" {
				assert.Equal(t, c.expStdout, stdoutBuf.String())
			}
			if c.expStderr != "" {
				assert.Equal(t, c.expStderr, stderrBuf.String())
			}
		})
	}
}

type noopWriteCloser struct{ io.Writer }

func (c *noopWriteCloser) Close() error { return nil }