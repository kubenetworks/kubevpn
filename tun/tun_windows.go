package tun

import (
	"context"
	"fmt"
	"github.com/datawire/dlib/derror"
	"github.com/pkg/errors"
	"golang.org/x/sys/windows"
	wireguardtun "golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
	"k8s.io/client-go/util/retry"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/go-log/log"
)

func createTun(cfg TunConfig) (conn net.Conn, itf *net.Interface, err error) {
	ip, ipNet, err := net.ParseCIDR(cfg.Addr)
	if err != nil {
		return
	}
	ifce, itf, err := openTun(context.Background())
	if err != nil {
		return
	}
	name, err := ifce.Name()

	cmd := fmt.Sprintf("netsh interface ip set address name=\"%s\" "+
		"source=static addr=%s mask=%s gateway=none",
		name, ip.String(), ipMask(ipNet.Mask))
	log.Log("[tun]", cmd)

	args := strings.Split(cmd, " ")
	err = retry.OnError(retry.DefaultRetry, func(err error) bool {
		return err != nil
	}, func() error {
		if er := exec.Command(args[0], args[1:]...).Run(); er != nil {
			return fmt.Errorf("%s: %v", cmd, er)
		}
		return nil
	})
	if err != nil {
		return
	}

	if err = addTunRoutes(name, cfg.Gateway, cfg.Routes...); err != nil {
		return
	}

	itf, err = net.InterfaceByName(name)
	if err != nil {
		return
	}

	conn = &winTunConn{
		ifce: ifce,
		addr: &net.IPAddr{IP: ip},
	}
	return
}

func openTun(ctx context.Context) (td wireguardtun.Device, p *net.Interface, err error) {
	defer func() {
		if r := recover(); r != nil {
			var ok bool
			if err, ok = r.(error); !ok {
				err = derror.PanicToError(r)
			}
		}
	}()
	interfaceName := "wg1"
	if td, err = wireguardtun.CreateTUN(interfaceName, 0); err != nil {
		return nil, nil, fmt.Errorf("failed to create TUN device: %w", err)
	}
	if _, err = td.Name(); err != nil {
		return nil, nil, fmt.Errorf("failed to get real name of TUN device: %w", err)
	}
	if i, err := winipcfg.LUID(td.(*wireguardtun.NativeTun).LUID()).Interface(); err != nil {
		return nil, nil, fmt.Errorf("failed to get interface for TUN device: %w", err)
	} else {
		if p, err = net.InterfaceByIndex(int(i.InterfaceIndex)); err != nil {
			return nil, nil, fmt.Errorf("failed to get interface for TUN device: %w", err)
		}
	}
	return td, p, nil
}

func (t *winTunConn) Close() error {
	// The tun.NativeTun device has a closing mutex which is read locked during
	// a call to Read(). The read lock prevents a call to Close() to proceed
	// until Read() actually receives something. To resolve that "deadlock",
	// we call Close() in one goroutine to wait for the lock and write a bogus
	// message in another that will be returned by Read().
	closeCh := make(chan error)
	go func() {
		// first message is just to indicate that this goroutine has started
		closeCh <- nil
		closeCh <- t.ifce.Close()
		close(closeCh)
	}()

	// Not 100%, but we can be fairly sure that Close() is
	// hanging on the lock, or at least will be by the time
	// the Read() returns
	<-closeCh
	return <-closeCh
}

func (t *winTunConn) getLUID() winipcfg.LUID {
	return winipcfg.LUID(t.ifce.(*wireguardtun.NativeTun).LUID())
}

func (t *winTunConn) addSubnet(_ context.Context, subnet *net.IPNet) error {
	return t.getLUID().AddIPAddress(*subnet)
}

func (t *winTunConn) removeSubnet(_ context.Context, subnet *net.IPNet) error {
	return t.getLUID().DeleteIPAddress(*subnet)
}

func (t *winTunConn) setDNS(ctx context.Context, server net.IP, domains []string) (err error) {
	ipFamily := func(ip net.IP) winipcfg.AddressFamily {
		f := winipcfg.AddressFamily(windows.AF_INET6)
		if ip4 := ip.To4(); ip4 != nil {
			f = windows.AF_INET
		}
		return f
	}
	family := ipFamily(server)
	luid := t.getLUID()
	if err = luid.SetDNS(family, []net.IP{server}, domains); err != nil {
		return err
	}
	_ = exec.CommandContext(ctx, "ipconfig", "/flushdns").Run()
	return nil
}

type winTunConn struct {
	ifce wireguardtun.Device
	addr net.Addr
}

func (c *winTunConn) Read(b []byte) (n int, err error) {
	return c.ifce.Read(b, 0)
}

func (c *winTunConn) Write(b []byte) (n int, err error) {
	return c.ifce.Write(b, 0)
}

func (c *winTunConn) LocalAddr() net.Addr {
	return c.addr
}

func (c *winTunConn) RemoteAddr() net.Addr {
	return &net.IPAddr{}
}

func (c *winTunConn) SetDeadline(t time.Time) error {
	return &net.OpError{Op: "set", Net: "tun", Source: nil, Addr: nil, Err: errors.New("deadline not supported")}
}

func (c *winTunConn) SetReadDeadline(t time.Time) error {
	return &net.OpError{Op: "set", Net: "tun", Source: nil, Addr: nil, Err: errors.New("deadline not supported")}
}

func (c *winTunConn) SetWriteDeadline(t time.Time) error {
	return &net.OpError{Op: "set", Net: "tun", Source: nil, Addr: nil, Err: errors.New("deadline not supported")}
}

func addTunRoutes(ifName string, gw string, routes ...IPRoute) error {
	for _, route := range routes {
		if route.Dest == nil {
			continue
		}

		deleteRoute(ifName, route.Dest.String())

		cmd := fmt.Sprintf("netsh interface ip add route prefix=%s interface=\"%s\" store=active",
			route.Dest.String(), ifName)
		if gw != "" {
			cmd += " nexthop=" + gw
		}
		log.Logf("[tun] %s", cmd)
		args := strings.Split(cmd, " ")
		if er := exec.Command(args[0], args[1:]...).Run(); er != nil {
			return fmt.Errorf("%s: %v", cmd, er)
		}
	}
	return nil
}

func deleteRoute(ifName string, route string) error {
	cmd := fmt.Sprintf("netsh interface ip delete route prefix=%s interface=\"%s\" store=active",
		route, ifName)
	args := strings.Split(cmd, " ")
	return exec.Command(args[0], args[1:]...).Run()
}

func ipMask(mask net.IPMask) string {
	return fmt.Sprintf("%d.%d.%d.%d", mask[0], mask[1], mask[2], mask[3])
}
