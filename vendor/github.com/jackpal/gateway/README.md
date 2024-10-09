# gateway

A simple library for discovering the IP address of the default gateway.

Example:

```go
package main

import (
    "fmt"

    "github.com/jackpal/gateway"
)

func main() {
    gateway, err := gateway.DiscoverGateway()
    if err != nil {
        fmt.Println(err)
    } else {
        fmt.Println("Gateway:", gateway.String())
   }
}
```

Provides implementations for:

+ Darwin (macOS)
+ Dragonfly
+ FreeBSD
+ Linux
+ NetBSD
+ OpenBSD
+ Solaris and illumos
+ Windows

Other platforms use an implementation that always returns an error.

Pull requests for other OSs happily considered!

## Versions

### v1.0.15

Update dependencies to latest versions. This was done to squelch a github security
alert caused by depending upon an old version of x/net. This is the first time I've
updated module versions, the tests pass, so hopefully everything's good.

### v1.0.14

+ [Fix panic when interface not set in Solaris `netstat -rn` output.](https://github.com/jackpal/gateway/pull/42)

### v1.0.13

+ Add tools/check-cross-compile.sh to check that the code compiles for various OSs.
+ Fix compilation errors exposed by tools/check-cross-compile.sh.

### v1.0.12

+ If there are multiple default gateways, Windows now returns the gateway with the lowest metric.
+ Fix solaris build break. (In theory, IDK how to test this easily.)
+ Upgrade to golang 1.21
+ Upgrade golang.org/x/net version, makes dependabot happy. Probably was not any actual security
  issue because gateway doesn't use any of the APIs of golang.org/x/net that had security issues.

### v1.0.11

+ Implement DiscoverInterface for BSD-like OSes.

### v1.0.10

+ Fix non-BSD-based builds.
  

### v1.0.9

+ Add go.mod and go.sum files.
+ Use "golang.org/x/net/route" to implement all BSD variants.
  + As a side effect this adds support for Dragonfly and NetBSD. 
+ Add example to README.
+ Remove broken continuous integration.

### v1.0.8

+ Add support for OpenBSD
+ Linux parser now supports gateways with an IP address of 0.0.0.0
+ Fall back to `netstat` on darwin systems if `route` fails.
+ Simplify Linux /proc/net/route parsers.
