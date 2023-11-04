package util

import (
	"fmt"
	"io"
	"os"

	"github.com/schollz/progressbar/v3"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

// SCP copy file to remote and exec command
func SCP(stdout, stderr io.Writer, conf *SshConfig, filename, to string, commands ...string) error {
	remote, err := DialSshRemote(conf)
	if err != nil {
		log.Errorf("Dial into remote server error: %s", err)
		return err
	}

	sess, err := remote.NewSession()
	if err != nil {
		return err
	}
	err = main(sess, stdout, stderr, filename, to)
	if err != nil {
		log.Errorf("Copy file to remote error: %s", err)
		return err
	}
	for _, command := range commands {
		sess, err = remote.NewSession()
		if err != nil {
			return err
		}
		output, err := sess.CombinedOutput(command)
		if err != nil {
			log.Error(string(output))
			return err
		} else {
			log.Info(string(output))
		}
	}
	return nil
}

// https://blog.neilpang.com/%E6%94%B6%E8%97%8F-scp-secure-copy%E5%8D%8F%E8%AE%AE/
func main(sess *ssh.Session, stdout, stderr io.Writer, filename string, to string) error {
	open, err := os.Open(filename)
	if err != nil {
		return err
	}
	stat, err := open.Stat()
	if err != nil {
		return err
	}
	defer open.Close()
	defer sess.Close()
	go func() {
		w, _ := sess.StdinPipe()
		defer w.Close()
		fmt.Fprintln(w, "D0755", 0, ".kubevpn") // mkdir
		fmt.Fprintln(w, "C0644", stat.Size(), to)
		err := sCopy(w, open, stat.Size(), stdout, stderr)
		if err != nil {
			log.Errorf("failed to transfer file to remote: %v", err)
			return
		}
		fmt.Fprint(w, "\x00") // transfer end with \x00
	}()
	return sess.Run("scp -tr ./")
}

func sCopy(dst io.Writer, src io.Reader, size int64, stdout, stderr io.Writer) error {
	total := float64(size) / 1024 / 1024
	log.Printf("Length: 68276642 (%0.2fM)\n", total)

	bar := progressbar.NewOptions(int(size),
		progressbar.OptionSetWriter(stdout),
		progressbar.OptionEnableColorCodes(true),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetWidth(50),
		progressbar.OptionOnCompletion(func() {
			_, _ = fmt.Fprint(stderr, "\n")
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
		log.Errorf("failed to transfer file to remote: %v", err)
		return err
	}
	if written != size {
		log.Errorf("failed to transfer file to remote: written size %d but actuall is %d", written, size)
		return err
	}
	return nil
}
