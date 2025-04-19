## Embedded WAF libraries

This directory contains Datadog's WAF static libraries taken from the releases
of https://github.com/DataDog/libddwaf

### Updating

From the root of the repository, run:

```console
./_tools/libddwaf-updater/update.sh
Will upgrade from v1.14.0 to v1.15.0
... downloaded <repo-root>/Datadog/go-libddwaf/include/ddwaf.h
... downloaded <repo-root>/Datadog/go-libddwaf/lib/darwin-arm64/libddwaf.dylib
... downloaded <repo-root>/Datadog/go-libddwaf/lib/darwin-amd64/libddwaf.dylib
... downloaded <repo-root>/Datadog/go-libddwaf/lib/linux-arm64/libddwaf.so
... downloaded <repo-root>/Datadog/go-libddwaf/lib/linux-amd64/libddwaf.so
... downloaded <repo-root>/Datadog/go-libddwaf/lib/linux-armv7/libddwaf.so
... downloaded <repo-root>/Datadog/go-libddwaf/lib/linux-i386/libddwaf.so
All done! Don't forget to check in changes to include/ and lib/, check the libddwaf upgrade guide to update bindings!
```
