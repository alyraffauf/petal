{
  perSystem = { pkgs, config, ... }: {
    packages.default = config.packages.petal;
    packages.petal = pkgs.buildGoModule {
      pname = "petal";
      version = "0-unstable";
      src = pkgs.lib.fileset.toSource {
        root = ../..;
        fileset = pkgs.lib.fileset.unions [
          ../../testdata
          (pkgs.lib.fileset.fileFilter (file:
            file.hasExt "go" || builtins.elem file.name [ "go.mod" "go.sum" ]
          ) ../..)
        ];
      };
      vendorHash = "sha256-Qgfvd5mNSW8GmU4X8DgqnR5K1nGCobxV2gazV+HQ6oM=";
      subPackages = [ "." ];
      # The Claude bridge uses the user's installed CLI, discovered at runtime.
      ldflags = [
        "-X main.bundledCodex=${pkgs.lib.getExe pkgs.codex-acp}"
        "-X main.bundledClaude=${pkgs.lib.getExe (pkgs.claude-agent-acp.overrideAttrs (_: { postInstall = ""; }))}"
      ];
      env.CGO_ENABLED = "0";
      meta.mainProgram = "petal";
    };
  };
}
