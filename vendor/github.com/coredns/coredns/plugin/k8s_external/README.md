# k8s_external

## Name

*k8s_external* - resolves load balancer, external IPs from outside Kubernetes clusters and if enabled headless services.

## Description

This plugin allows an additional zone to resolve the external IP address(es) of a Kubernetes
service and headless services. This plugin is only useful if the *kubernetes* plugin is also loaded.

The plugin uses an external zone to resolve in-cluster IP addresses. It only handles queries for A,
AAAA, SRV, and PTR records; To make it a proper DNS zone, it handles SOA and NS queries for the apex of the zone.

By default the apex of the zone will look like the following (assuming the zone used is `example.org`):

~~~ dns
example.org.	5 IN	SOA ns1.dns.example.org. hostmaster.example.org. (
				12345      ; serial
				14400      ; refresh (4 hours)
				3600       ; retry (1 hour)
				604800     ; expire (1 week)
				5          ; minimum (4 hours)
				)
example.org		5 IN	NS ns1.dns.example.org.

ns1.dns.example.org.  5 IN  A    ....
ns1.dns.example.org.  5 IN  AAAA ....
~~~

Note that we use the `dns` subdomain for the records DNS needs (see the `apex` directive). Also
note the SOA's serial number is static. The IP addresses of the nameserver records are those of the
CoreDNS service.

The *k8s_external* plugin handles the subdomain `dns` and the apex of the zone itself; all other
queries are resolved to addresses in the cluster.

## Syntax

~~~
k8s_external [ZONE...]
~~~

* **ZONES** zones *k8s_external* should be authoritative for.

If you want to change the apex domain or use a different TTL for the returned records you can use
this extended syntax.

~~~
k8s_external [ZONE...] {
    apex APEX
    ttl TTL
}
~~~

* **APEX** is the name (DNS label) to use for the apex records; it defaults to `dns`.
* `ttl` allows you to set a custom **TTL** for responses. The default is 5 (seconds).

If you want to enable headless service resolution, you can do so by adding `headless` option.

~~~
k8s_external [ZONE...] {
    headless
}
~~~

* if there is a headless service with external IPs set, external IPs will be resolved

If the queried domain does not exist, you can fall through to next plugin by adding the `fallthrough` option.

~~~
k8s_external [ZONE...] {
    fallthrough [ZONE...]
}
~~~

## Examples

Enable names under `example.org` to be resolved to in-cluster DNS addresses.

~~~
. {
   kubernetes cluster.local
   k8s_external example.org
}
~~~

With the Corefile above, the following Service will get an `A` record for `test.default.example.org` with the IP address `192.168.200.123`.

~~~
apiVersion: v1
kind: Service
metadata:
 name: test
 namespace: default
spec:
 clusterIP: None
 externalIPs:
 - 192.168.200.123
 type: ClusterIP
~~~

The *k8s_external* plugin can be used in conjunction with the *transfer* plugin to enable
zone transfers.  Notifies are not supported.

 ~~~
     . {
         transfer example.org {
             to *
         }
         kubernetes cluster.local
         k8s_external example.org
     }
 ~~~

With the `fallthrough` option, if the queried domain does not exist, it will be passed to the next plugin that matches the zone.

~~~
. {
   kubernetes cluster.local
   k8s_external example.org {
     fallthrough
   }
   forward . 8.8.8.8
}
~~~

# See Also

For some background see [resolve external IP address](https://github.com/kubernetes/dns/issues/242).
And [A records for services with Load Balancer IP](https://github.com/coredns/coredns/issues/1851).

