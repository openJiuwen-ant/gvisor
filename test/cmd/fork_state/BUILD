load("//tools:defs.bzl", "cc_binary")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

cc_binary(
    name = "fork_state",
    testonly = True,
    srcs = ["fork_state.c"],
    linkopts = ["-static"],
    linkstatic = True,
    visibility = ["//runsc/container:__pkg__"],
)
