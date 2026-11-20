{ self, inputs, ... }:
{
  flake.homeManagerModules = {
    default = self.homeManagerModules.petal;
    petal = import ../modules/petal.nix self;
  };

  perSystem = { pkgs, ... }: {
    checks.home-manager = (inputs.home-manager.lib.homeManagerConfiguration {
      inherit pkgs;
      modules = [
        self.homeManagerModules.petal
        {
          home.username = "petal-test";
          home.homeDirectory = if pkgs.stdenv.hostPlatform.isDarwin then "/Users/petal-test" else "/home/petal-test";
          home.stateVersion = "26.05";
          programs.mcp.enable = true;
          programs.mcp.servers.test.command = "test-mcp";
          programs.mcp.servers.remote = {
            url = "https://example.org/mcp";
            headers.Authorization = "{env:TEST_AUTHORIZATION}";
          };
          programs.petal = {
            enable = true;
            enableMcpIntegration = true;
            settings.accounts = [ { name = "test"; usage = [ "test-quota" ]; } ];
          };
        }
      ];
    }).activationPackage;
  };
}
