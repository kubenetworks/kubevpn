package regctl

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/regclient/regclient"
	"github.com/regclient/regclient/config"
	"github.com/regclient/regclient/types/ref"

	"github.com/wencaiwulue/kubevpn/v2/pkg/util/regctl/ascii"
)

var digestRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*(?:[-_+.][A-Za-z][A-Za-z0-9]*)*:[[:xdigit:]]{32,}$`)

// parseImageDSN extracts inline credentials from a DSN-style image reference.
// Format: [user[:pass]@]registry[:port]/repo[:tag][@digest]
func parseImageDSN(raw string) (imageRef, user, pass string) {
	imageRef = raw

	at := strings.Index(raw, "@")
	if at < 0 {
		return
	}

	left := raw[:at]
	right := raw[at+1:]

	// left contains "/" → it's registry/repo, not credentials (e.g. registry.com/repo@sha256:abc)
	if strings.Contains(left, "/") {
		return
	}

	// right is a bare digest with no "/" → standard digest ref (e.g. repo@sha256:abc)
	if !strings.Contains(right, "/") && digestRE.MatchString(right) {
		return
	}

	// left is credentials, right is the image reference
	imageRef = right

	if i := strings.Index(left, ":"); i >= 0 {
		user = left[:i]
		pass = left[i+1:]
	} else {
		user = left
	}

	return
}

// extractRegistry returns the registry host[:port] from an image reference.
func extractRegistry(imageRef string) string {
	r, err := ref.New(imageRef)
	if err != nil {
		i := strings.Index(imageRef, "/")
		if i > 0 {
			return imageRef[:i]
		}
		return imageRef
	}
	return r.Registry
}

// probeRegistryTLS probes the registry to determine TLS support.
// It tries HTTPS first, then falls back to HTTP if the TLS handshake fails.
func probeRegistryTLS(ctx context.Context, registry string) config.TLSConf {
	host := registry
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "443")
	}

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", host, &tls.Config{InsecureSkipVerify: true})
	if err == nil {
		conn.Close()
		return config.TLSInsecure
	}

	// TLS handshake failed — check if plain HTTP works
	httpHost := registry
	if _, _, err := net.SplitHostPort(httpHost); err != nil {
		httpHost = net.JoinHostPort(httpHost, "80")
	}
	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Get(fmt.Sprintf("http://%s/v2/", httpHost))
	if err == nil {
		resp.Body.Close()
		return config.TLSDisabled
	}

	return config.TLSEnabled
}

func buildHostConfig(ctx context.Context, imageRef, user, pass string) *config.Host {
	registry := extractRegistry(imageRef)
	host := config.HostNewName(imageRef)
	host.User = user
	host.Pass = pass
	host.TLS = probeRegistryTLS(ctx, registry)
	return host
}

// TransferImageWithRegctl copies a container image from imageSource to imageTarget using regclient with progress display.
// Image references support DSN-style inline credentials: user:pass@registry.com/repo:tag
func TransferImageWithRegctl(ctx context.Context, imageSource, imageTarget string) error {
	srcRef, srcUser, srcPass := parseImageDSN(imageSource)
	dstRef, dstUser, dstPass := parseImageDSN(imageTarget)

	var rcOpts []regclient.Opt
	rcOpts = append(rcOpts,
		regclient.WithDockerCerts(),
		regclient.WithDockerCreds(),
		regclient.WithSlog(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))),
	)
	if srcUser != "" {
		rcOpts = append(rcOpts, regclient.WithConfigHost(*buildHostConfig(ctx, srcRef, srcUser, srcPass)))
	}
	if dstUser != "" {
		rcOpts = append(rcOpts, regclient.WithConfigHost(*buildHostConfig(ctx, dstRef, dstUser, dstPass)))
	}

	rc := regclient.New(rcOpts...)

	src, err := ref.New(srcRef)
	if err != nil {
		_, _ = os.Stdout.Write([]byte(fmt.Sprintf("failed to create ref: %v\n", err)))
		return err
	}
	defer rc.Close(ctx, src)
	dst, err := ref.New(dstRef)
	if err != nil {
		_, _ = os.Stdout.Write([]byte(fmt.Sprintf("failed to create ref: %v\n", err)))
		return err
	}
	defer rc.Close(ctx, dst)

	// check for a tty and attach progress reporter
	done := make(chan bool)
	progress := &ImageProgress{
		Start:    time.Now(),
		Entries:  map[string]*ImageProgressEntry{},
		AsciiOut: ascii.NewLines(os.Stdout),
		Bar:      ascii.NewProgressBar(os.Stdout),
	}
	progressFreq := time.Millisecond * 250
	ticker := time.NewTicker(progressFreq)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-done:
				ticker.Stop()
				return
			case <-ticker.C:
				progress.Display(false)
			}
		}
	}()
	var opts []regclient.ImageOpts
	opts = append(opts, regclient.ImageWithCallback(progress.Callback))

	err = rc.ImageCopy(ctx, src, dst, opts...)

	close(done)
	progress.Display(true)

	return err
}
