package auto

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/metrics"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/plugin/pkg/upstream"
	"github.com/coredns/coredns/plugin/transfer"
)

var log = clog.NewWithPlugin("auto")

func init() { plugin.Register("auto", setup) }

func setup(c *caddy.Controller) error {
	a, err := autoParse(c)
	if err != nil {
		return plugin.Error("auto", err)
	}

	c.OnStartup(func() error {
		m := dnsserver.GetConfig(c).Handler("prometheus")
		if m != nil {
			(&a).metrics = m.(*metrics.Metrics)
		}
		t := dnsserver.GetConfig(c).Handler("transfer")
		if t != nil {
			(&a).transfer = t.(*transfer.Transfer)
		}
		return nil
	})

	walkChan := make(chan bool)

	c.OnStartup(func() error {
		err := a.Walk()
		if err != nil {
			return err
		}
		if err := a.Notify(); err != nil {
			log.Warning(err)
		}
		if a.loader.ReloadInterval == 0 {
			return nil
		}
		go func() {
			ticker := time.NewTicker(a.loader.ReloadInterval)
			defer ticker.Stop()
			for {
				select {
				case <-walkChan:
					return
				case <-ticker.C:
					a.Walk()
					if err := a.Notify(); err != nil {
						log.Warning(err)
					}
				}
			}
		}()
		return nil
	})

	c.OnShutdown(func() error {
		close(walkChan)
		return nil
	})

	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		a.Next = next
		return a
	})

	return nil
}

func autoParse(c *caddy.Controller) (Auto, error) {
	nilInterval := -1 * time.Second
	var a = Auto{
		loader: loader{
			template:       "${1}",
			re:             regexp.MustCompile(`db\.(.*)`),
			ReloadInterval: nilInterval,
		},
		Zones: &Zones{},
	}

	config := dnsserver.GetConfig(c)

	for c.Next() {
		// auto [ZONES...]
		args := c.RemainingArgs()
		a.Zones.origins = plugin.OriginsFromArgsOrServerBlock(args, c.ServerBlockKeys)
		a.loader.upstream = upstream.New()

		for c.NextBlock() {
			switch c.Val() {
			case "directory": // directory DIR [REGEXP TEMPLATE]
				if !c.NextArg() {
					return a, c.ArgErr()
				}
				a.loader.directory = c.Val()
				if !filepath.IsAbs(a.loader.directory) && config.Root != "" {
					a.loader.directory = filepath.Join(config.Root, a.loader.directory)
				}
				_, err := os.Stat(a.loader.directory)
				if err != nil {
					if os.IsNotExist(err) {
						log.Warningf("Directory does not exist: %s", a.loader.directory)
					} else {
						return a, c.Errf("Unable to access root path '%s': %v", a.loader.directory, err)
					}
				}

				// regexp template
				if c.NextArg() {
					a.loader.re, err = regexp.Compile(c.Val())
					if err != nil {
						return a, err
					}
					if a.loader.re.NumSubexp() == 0 {
						return a, c.Errf("Need at least one sub expression")
					}

					if !c.NextArg() {
						return a, c.ArgErr()
					}
					a.loader.template = rewriteToExpand(c.Val())
				}

				if c.NextArg() {
					return Auto{}, c.ArgErr()
				}

			case "reload":
				t := c.RemainingArgs()
				if len(t) < 1 {
					return a, errors.New("reload duration value is expected")
				}
				d, err := time.ParseDuration(t[0])
				if d < 0 {
					err = errors.New("invalid duration")
				}
				if err != nil {
					return a, plugin.Error("file", err)
				}
				a.loader.ReloadInterval = d

			case "upstream":
				// remove soon
				c.RemainingArgs() // eat remaining args

			default:
				return Auto{}, c.Errf("unknown property '%s'", c.Val())
			}
		}
	}

	if a.loader.ReloadInterval == nilInterval {
		a.loader.ReloadInterval = 60 * time.Second
	}

	return a, nil
}
