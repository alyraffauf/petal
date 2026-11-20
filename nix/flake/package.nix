{
  perSystem = { pkgs, config, ... }:
  let
    claudeAdapter = pkgs.claude-agent-acp.overrideAttrs (old: {
      # Use the CLI discovered at runtime. Version 0.100.0 predates the
      # replay_complete boundary in the ACP v2 draft.
      postInstall = pkgs.lib.optionalString (old.version == "0.100.0") ''
        substituteInPlace "$out/lib/node_modules/@agentclientprotocol/claude-agent-acp/dist/v2/agent.js" \
          --replace-fail 'const result = await restore();' 'const result = await restore();
            await this.send({ sessionId, update: { sessionUpdate: "replay_complete" } });'
      '';
    });
  in {
    packages.default = config.packages.petal;
    packages.petal = pkgs.buildGoModule {
      pname = "petal";
      version = "0-unstable";
      src = pkgs.lib.fileset.toSource {
        root = ../..;
        fileset = pkgs.lib.fileset.unions [
          ../../internal/tui/testdata
          ../../internal/tui/orchid.png
          ../../internal/sessions/migrations
          (pkgs.lib.fileset.fileFilter (file:
            file.hasExt "go" || builtins.elem file.name [ "go.mod" "go.sum" "LICENSE" ]
          ) ../..)
        ];
      };
      vendorHash = "sha256-lJ8+MVvmAgh8iuOA5LnXPXytCXbWJl1xvNVpFxWoTYM=";
      # The Claude bridge uses the user's installed CLI, discovered at runtime.
      ldflags = [
        "-X petal/internal/routing.bundledCodex=${pkgs.lib.getExe pkgs.codex-acp}"
        "-X petal/internal/routing.bundledClaude=${pkgs.lib.getExe claudeAdapter}"
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
