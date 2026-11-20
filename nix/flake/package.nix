{
  perSystem = { pkgs, config, ... }: {
    packages.default = config.packages.petal;
    packages.petal = pkgs.buildGoModule {
      pname = "petal";
      version = "0-unstable";
      src = pkgs.lib.fileset.toSource {
        root = ../..;
        fileset = pkgs.lib.fileset.unions [
          ../../internal/tui/testdata
          (pkgs.lib.fileset.fileFilter (file:
            file.hasExt "go" || builtins.elem file.name [ "go.mod" "go.sum" "LICENSE" ]
          ) ../..)
        ];
      };
      vendorHash = "sha256-fOeoJ/TtJzH6+BHZTm2wHCEwbdkd57CxnHNUO9sAcE4=";
      # The Claude bridge uses the user's installed CLI, discovered at runtime.
      ldflags = [
        "-X petal/internal/routing.bundledCodex=${pkgs.lib.getExe pkgs.codex-acp}"
        "-X petal/internal/routing.bundledClaude=${pkgs.lib.getExe (pkgs.claude-agent-acp.overrideAttrs (_: { postInstall = ""; }))}"
      ];
      env.CGO_ENABLED = "0";
      postInstall = ''
        install -Dm444 LICENSE "$out/share/licenses/petal/LICENSE"
      '';
      meta.license = pkgs.lib.licenses.mit;
      meta.mainProgram = "petal";
    };
  };
}
