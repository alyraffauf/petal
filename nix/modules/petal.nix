self:
{ config, lib, pkgs, ... }:
let
  cfg = config.programs.petal;
  toml = pkgs.formats.toml { };
  file = "petal/config.toml";
  source = toml.generate "petal-config.toml" cfg.settings;
in {
  options.programs.petal = {
    enable = lib.mkEnableOption "Petal";
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
    (lib.mkIf (cfg.mutableSettings && cfg.settings != { }) {
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
    (lib.mkIf (!cfg.mutableSettings && cfg.settings != { }) {
      xdg.configFile.${file}.source = source;
      home.activation.petalImmutableSettings = lib.hm.dag.entryBetween [ "linkGeneration" ] [ "writeBoundary" ] (
        lib.hm.generators.mkImpureConfigCleanup { file = config.xdg.configFile.${file}; }
      );
    })
  ]);
}
