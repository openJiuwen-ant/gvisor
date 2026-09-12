load("//tools:defs.bzl", "cc_binary", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

cc_binary(
    name = "templatestub_bin",
    srcs = ["stub.c"],
    linkopts = ["-static"],
    linkstatic = True,
)

genrule(
    name = "templatestub_embed",
    srcs = [":templatestub_bin"],
    outs = ["templatestub.bin"],
    cmd = "cp $(SRCS) $(OUTS)",
)

go_library(
    name = "templatestub",
    srcs = ["templatestub.go"],
    embedsrcs = ["templatestub.bin"],
    visibility = [
        "//runsc:__subpackages__",
    ],
    deps = [
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "templatestub_test",
    srcs = ["templatestub_test.go"],
    library = ":templatestub",
    deps = [
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
