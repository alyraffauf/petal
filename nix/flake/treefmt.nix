{inputs, ...}: {
  imports = [inputs.treefmt-nix.flakeModule];

  # `nix fmt` formats the tree. `nix flake check` fails on unformatted files.
  perSystem.treefmt = {
    projectRootFile = "flake.nix";
    programs.gofmt.enable = true;
    programs.alejandra.enable = true;
    programs.prettier.enable = true;
    # Generated code keeps the layout its generator wrote. The website formats
    # itself with `npm run format`, which loads the Astro plugin.
    settings.global.excludes = [
      "internal/sessions/sessiondb/*"
      "website/*"
    ];
  };
}
