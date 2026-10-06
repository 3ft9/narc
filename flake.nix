{
  description = "narc: ephemeral GitHub Actions runners on Nomad";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAll = f: nixpkgs.lib.genAttrs systems (s: f nixpkgs.legacyPackages.${s});
    in
    {
      packages = forAll (pkgs: rec {
        narc = pkgs.buildGoModule {
          pname = "narc";
          version = self.shortRev or "dirty";
          src = self;
          vendorHash = "sha256-rxiYyMGXWiT/E7OI1yVOc7YLPpbl01xhS6ebu1v9y0o=";
          ldflags = [ "-s" "-w" "-X main.version=${self.shortRev or "dirty"}" ];
          meta.mainProgram = "narc";
        };
        default = narc;
      });

      # Node-side bits: QEMU for the Nomad agent, the cni/narc network, the
      # egress firewall and the image cache host volume.
      nixosModules.node = import ./nix/node.nix;
      nixosModules.default = self.nixosModules.node;
    };
}
