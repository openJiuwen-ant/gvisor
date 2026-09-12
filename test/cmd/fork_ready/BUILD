load("//tools:defs.bzl", "cc_binary")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

cc_binary(
    name = "fork_ready",
    testonly = True,
    srcs = ["fork_ready.c"],
    linkopts = ["-static"],
    linkstatic = True,
    visibility = ["//runsc/container:__pkg__"],
)
