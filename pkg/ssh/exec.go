package ssh

import (
	"bytes"

	gossh "golang.org/x/crypto/ssh"
)

// RemoteRun executes a command on the remote SSH server and returns stdout and stderr output.
func RemoteRun(client *gossh.Client, cmd string, env map[string]string) (output []byte, errOut []byte, err error) {
	var session *gossh.Session
	session, err = client.NewSession()
	if err != nil {
		return
	}
	defer session.Close()
	for k, v := range env {
		// /etc/ssh/sshd_config
		// AcceptEnv DEBIAN_FRONTEND
		_ = session.Setenv(k, v)
	}
	var out bytes.Buffer
	var er bytes.Buffer
	session.Stdout = &out
	session.Stderr = &er
	err = session.Run(cmd)
	return out.Bytes(), er.Bytes(), err
}
