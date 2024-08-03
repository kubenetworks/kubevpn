package ssh

import (
	"fmt"
	"io"
	"os"

	"github.com/schollz/progressbar/v3"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

// SCPAndExec copy file to remote and exec command
func SCPAndExec(stdout, stderr io.Writer, client *ssh.Client, filename, to string, commands ...string) error {
	err := SCP(client, stdout, stderr, filename, to)
	if err != nil {
		log.Errorf("Copy file to remote error: %s", err)
		return err
	}
	for _, command := range commands {
		var session *ssh.Session
		session, err = client.NewSession()
		if err != nil {
			return err
		}
		output, err := session.CombinedOutput(command)
		if err != nil {
			log.Error(string(output))
			return err
		} else {
			log.Info(string(output))
		}
	}
	return nil
}

// SCP https://blog.neilpang.com/%E6%94%B6%E8%97%8F-scp-secure-copy%E5%8D%8F%E8%AE%AE/
func SCP(client *ssh.Client, stdout, stderr io.Writer, filename, to string) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	go func() {
		w, _ := sess.StdinPipe()
		defer w.Close()
		fmt.Fprintln(w, "D0755", 0, ".kubevpn") // mkdir
		fmt.Fprintln(w, "C0644", stat.Size(), to)
		err := sCopy(w, file, stat.Size(), stdout, stderr)
		if err != nil {
			log.Errorf("Failed to transfer file to remote: %v", err)
			return
		}
		fmt.Fprint(w, "\x00") // transfer end with \x00
	}()
	return sess.Run("scp -tr ./")
}

func sCopy(dst io.Writer, src io.Reader, size int64, stdout, stderr io.Writer) error {
	total := float64(size) / 1024 / 1024
	s := fmt.Sprintf("Length: %d (%0.2fM)", size, total)
	io.WriteString(stdout, s+"\n")

	bar := progressbar.NewOptions(int(size),
		progressbar.OptionSetWriter(stdout),
		progressbar.OptionEnableColorCodes(true),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetWidth(25),
		progressbar.OptionOnCompletion(func() {
			_, _ = fmt.Fprint(stderr, "\n\r")
		}),
		progressbar.OptionSetRenderBlankState(true),
		progressbar.OptionSetDescription("Transferring file..."),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "=",
			SaucerHead:    ">",
			SaucerPadding: " ",
			BarStart:      "[",
			BarEnd:        "]",
		}))
	buf := make([]byte, 10<<(10*2)) // 10M
	written, err := io.CopyBuffer(io.MultiWriter(dst, bar), src, buf)
	if err != nil {
		log.Errorf("Failed to transfer file to remote: %v", err)
		return err
	}
	if written != size {
		log.Errorf("Failed to transfer file to remote: written size %d but actuall is %d", written, size)
		return err
	}
	return nil
}
