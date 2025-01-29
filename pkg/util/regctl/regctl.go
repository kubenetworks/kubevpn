package regctl

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/regclient/regclient"
	"github.com/regclient/regclient/types/ref"

	"github.com/wencaiwulue/kubevpn/v2/pkg/util/regctl/ascii"
)

func TransferImageWithRegctl(ctx context.Context, imageSource, imageTarget string) error {
	rc := regclient.New(
		regclient.WithDockerCerts(),
		regclient.WithDockerCreds(),
		regclient.WithSlog(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))),
	)
	// create a reference for an image
	src, err := ref.New(imageSource)
	if err != nil {
		_, _ = os.Stdout.Write([]byte(fmt.Sprintf("failed to create ref: %v\n", err)))
		return err
	}
	defer rc.Close(ctx, src)
	dst, err := ref.New(imageTarget)
	if err != nil {
		_, _ = os.Stdout.Write([]byte(fmt.Sprintf("failed to create ref: %v\n", err)))
		return err
	}
	defer rc.Close(ctx, dst)

	// check for a tty and attach progress reporter
	done := make(chan bool)
	var progress = &ImageProgress{
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
