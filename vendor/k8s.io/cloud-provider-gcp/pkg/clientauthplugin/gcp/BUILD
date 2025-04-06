package(default_visibility = ["//visibility:public"])

load(
    "@io_bazel_rules_go//go:def.bzl",
    "go_library",
    "go_test",
)
load("//defs:version.bzl", "version_x_defs")

go_library(
    name = "gcp",
    srcs = ["gcp.go"],
    importpath = "k8s.io/cloud-provider-gcp/pkg/clientauthplugin/gcp",
    deps = [
        "//vendor/golang.org/x/oauth2",
        "//vendor/golang.org/x/oauth2/google",
        "//vendor/k8s.io/apimachinery/pkg/util/net",
        "//vendor/k8s.io/apimachinery/pkg/util/yaml",
        "//vendor/k8s.io/client-go/rest",
        "//vendor/k8s.io/client-go/util/jsonpath",
        "//vendor/k8s.io/klog/v2:klog",
    ],
)

go_test(
    name = "gcp_test",
    srcs = ["gcp_test.go"],
    embed = [":gcp"],
    deps = ["//vendor/golang.org/x/oauth2"],
)
