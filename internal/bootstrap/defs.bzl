"""Assemble bootstrap core with one build-specific implementation of New."""

load("@rules_go//go:def.bzl", "go_library")

def bootstrap_library(name, implementation = None, deps = [], **kwargs):
    """Compile shared startup with the public default or a private source file."""
    if implementation == None:
        implementation = Label("//internal/bootstrap:default_bootstrap.go")
        deps = deps + [
            Label("//api/v2:api"),
            Label("//internal/handlers/v2:handlers"),
            Label("@com_github_gin_gonic_gin//:go_default_library"),
        ]
    go_library(
        name = name,
        srcs = [Label("//internal/bootstrap:core_srcs"), implementation],
        importpath = "github.com/kubev2v/assisted-migration-agent/internal/bootstrap",
        deps = [
            Label("//internal/config"),
            Label("//internal/handlers"),
            Label("//internal/models"),
            Label("//internal/server"),
            Label("//internal/services"),
            Label("//internal/store"),
            Label("//internal/store/migrations"),
            Label("//pkg/console"),
            Label("//pkg/crypto"),
            Label("@com_github_gin_gonic_gin//binding:go_default_library"),
            Label("@com_github_go_playground_validator_v10//:go_default_library"),
            Label("@com_github_google_uuid//:go_default_library"),
            Label("@com_github_kubev2v_migration_planner//pkg/opa:go_default_library"),
            Label("@org_uber_go_zap//:go_default_library"),
        ] + deps,
        **kwargs
    )
