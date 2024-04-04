package grpc

import (
	"crypto/tls"
	"fmt"
	"path/filepath"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/parse"
	pkgtls "github.com/coredns/coredns/plugin/pkg/tls"
)

func init() { plugin.Register("grpc", setup) }

func setup(c *caddy.Controller) error {
	g, err := parseGRPC(c)
	if err != nil {
		return plugin.Error("grpc", err)
	}

	if g.len() > max {
		return plugin.Error("grpc", fmt.Errorf("more than %d TOs configured: %d", max, g.len()))
	}

	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		g.Next = next // Set the Next field, so the plugin chaining works.
		return g
	})

	return nil
}

func parseGRPC(c *caddy.Controller) (*GRPC, error) {
	var (
		g   *GRPC
		err error
		i   int
	)
	for c.Next() {
		if i > 0 {
			return nil, plugin.ErrOnce
		}
		i++
		g, err = parseStanza(c)
		if err != nil {
			return nil, err
		}
	}
	return g, nil
}

func parseStanza(c *caddy.Controller) (*GRPC, error) {
	g := newGRPC()

	if !c.Args(&g.from) {
		return g, c.ArgErr()
	}
	normalized := plugin.Host(g.from).NormalizeExact()
	if len(normalized) == 0 {
		return g, fmt.Errorf("unable to normalize '%s'", g.from)
	}
	g.from = normalized[0] // only the first is used.

	to := c.RemainingArgs()
	if len(to) == 0 {
		return g, c.ArgErr()
	}

	toHosts, err := parse.HostPortOrFile(to...)
	if err != nil {
		return g, err
	}

	for c.NextBlock() {
		if err := parseBlock(c, g); err != nil {
			return g, err
		}
	}

	if g.tlsServerName != "" {
		if g.tlsConfig == nil {
			g.tlsConfig = new(tls.Config)
		}
		g.tlsConfig.ServerName = g.tlsServerName
	}
	for _, host := range toHosts {
		pr, err := newProxy(host, g.tlsConfig)
		if err != nil {
			return nil, err
		}
		g.proxies = append(g.proxies, pr)
	}

	return g, nil
}

func parseBlock(c *caddy.Controller, g *GRPC) error {
	switch c.Val() {
	case "except":
		ignore := c.RemainingArgs()
		if len(ignore) == 0 {
			return c.ArgErr()
		}
		for i := 0; i < len(ignore); i++ {
			g.ignored = append(g.ignored, plugin.Host(ignore[i]).NormalizeExact()...)
		}
	case "tls":
		args := c.RemainingArgs()
		if len(args) > 3 {
			return c.ArgErr()
		}

		for i := range args {
			if !filepath.IsAbs(args[i]) && dnsserver.GetConfig(c).Root != "" {
				args[i] = filepath.Join(dnsserver.GetConfig(c).Root, args[i])
			}
		}
		tlsConfig, err := pkgtls.NewTLSConfigFromArgs(args...)
		if err != nil {
			return err
		}
		g.tlsConfig = tlsConfig
	case "tls_servername":
		if !c.NextArg() {
			return c.ArgErr()
		}
		g.tlsServerName = c.Val()
	case "policy":
		if !c.NextArg() {
			return c.ArgErr()
		}
		switch x := c.Val(); x {
		case "random":
			g.p = &random{}
		case "round_robin":
			g.p = &roundRobin{}
		case "sequential":
			g.p = &sequential{}
		default:
			return c.Errf("unknown policy '%s'", x)
		}
	default:
		if c.Val() != "}" {
			return c.Errf("unknown property '%s'", c.Val())
		}
	}

	return nil
}

const max = 15 // Maximum number of upstreams.
