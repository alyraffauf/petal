self:
{ config, lib, pkgs, ... }:
let
  cfg = config.programs.petal;
  toml = pkgs.formats.toml { };
  file = "petal/config.toml";
  explicitServers = cfg.settings.mcp_servers or [ ];
  explicitNames = map (server: server.name) explicitServers;
  sharedServers = lib.filterAttrs (name: server:
    server.command != null
    && lib.hm.mcp.resolveEnabled server != false
    && !(builtins.elem name explicitNames)
  ) config.programs.mcp.servers;
  importServer = name: definition:
    let server = lib.hm.mcp.wrapEnvFilesCommand { inherit pkgs name; } definition;
    in {
      inherit name;
      command = toString server.command;
      inherit (server) args;
      env = lib.mapAttrsToList (name: value: { inherit name value; }) server.env;
    };
  settings = cfg.settings // lib.optionalAttrs (cfg.enableMcpIntegration && config.programs.mcp.enable) {
    mcp_servers = explicitServers ++ lib.mapAttrsToList importServer sharedServers;
  };
  source = toml.generate "petal-config.toml" settings;
in {
  options.programs.petal = {
    enable = lib.mkEnableOption "Petal";
    enableMcpIntegration = lib.mkEnableOption "importing enabled stdio servers from programs.mcp.servers";
    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.petal;
      defaultText = lib.literalExpression "petal.packages.${pkgs.stdenv.hostPlatform.system}.petal";
      description = "Petal package to install.";
    };
    mutableSettings = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Keep the TOML file writable and merge declared settings over local values at activation.";
    };
    settings = lib.mkOption {
      type = toml.type;
      default = { };
      description = "Settings written to Petal's TOML configuration.";
    };
  };
  config = lib.mkIf cfg.enable (lib.mkMerge [
    { home.packages = [ cfg.package ]; }
    (lib.mkIf (cfg.mutableSettings && settings != { }) {
      home.activation.petalSettings = lib.hm.dag.entryAfter [ "linkGeneration" ] (
        lib.hm.generators.mkImpureConfigMerger {
          inherit pkgs;
          format = "toml";
          empty = "{}";
          jqOperation = "$dynamic * $static";
          path = "${config.xdg.configHome}/${file}";
          staticSettings = source;
          mode = "600";
        }
      );
    })
    (lib.mkIf (!cfg.mutableSettings && settings != { }) {
      xdg.configFile.${file}.source = source;
      home.activation.petalImmutableSettings = lib.hm.dag.entryBetween [ "linkGeneration" ] [ "writeBoundary" ] (
        lib.hm.generators.mkImpureConfigCleanup { file = config.xdg.configFile.${file}; }
      );
    })
  ]);
}
