# CLI Examples Test Fixture

This file has various CLI examples, some valid and some invalid.

## Valid examples

These should pass validation:

```shell cli-checked
kubevpn sync deployment/productpage --sync ~/code:/code/app
```

```shell cli-checked
kubevpn sync deployment/productpage --sync ~/code:/code/app --headers foo=bar
```

```shell cli-checked
kubevpn disconnect --all
```

```shell cli-checked
kubevpn disconnect 03dc50feb8c3
```

## Invalid examples

These should fail validation:

```shell cli-checked
kubevpn sync deployment/productpage --local-dir ~/code
```

```shell cli-checked
kubevpn disconnect
```

```shell cli-checked
kubevpn sync deployment/productpage --bad-flag
```

```shell cli-checked
kubevpn sync deployment/productpage --sync ~/code --another-bad-flag
```

## Valid examples with various flags

These should pass validation:

```shell cli-checked
kubevpn sync deployment/productpage --sync ~/code:/code/app --headers foo=bar
```

```shell cli-checked
kubevpn sync deployment/productpage --sync ~/code:/code/app --ssh-addr 192.168.1.1:22 --ssh-username root --ssh-keyfile ~/.ssh/id_rsa
```

```shell cli-checked
kubevpn disconnect --all
```

```shell cli-checked
kubevpn disconnect abc123
```

## Not checked examples

These don't have the cli-checked marker, so they should be ignored:

```shell
kubevpn disconnect invalid-flag-that-should-not-be-checked
```
