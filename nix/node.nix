# NixOS module for a Nomad client that runs narc runner VMs.
#
#   imports = [ narc.nixosModules.node ];
#   services.narc-node = {
#     enable = true;
#     nodeAddresses = [ "203.0.113.1" "203.0.113.2" "203.0.113.3" ];
#   };
#
# Needs networking.nftables.enable = true for the egress rules.
{ config, lib, pkgs, ... }:
let
  cfg = config.services.narc-node;
  conflist = pkgs.writeText "narc.conflist" (builtins.toJSON {
    cniVersion = "1.0.0";
    name = "narc";
    plugins = [
      { type = "loopback"; }
      {
        type = "bridge";
        bridge = "narc";
        isGateway = true;
        ipMasq = true;
        ipam = {
          type = "host-local";
          ranges = [ [ { subnet = cfg.subnet; } ] ];
          routes = [ { dst = "0.0.0.0/0"; } ];
        };
      }
    ];
  });
  nft = builtins.readFile ../deploy/narc.nft;
  addrs = lib.concatStringsSep ", " cfg.nodeAddresses;
in
{
  options.services.narc-node = {
    enable = lib.mkEnableOption "narc runner VM support on this Nomad client";
    nodeAddresses = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "Public IPv4 addresses of every Nomad node; runner VMs may not reach them.";
    };
    subnet = lib.mkOption {
      type = lib.types.str;
      default = "172.31.250.0/24";
      description = "Subnet for the narc CNI bridge.";
    };
    cacheDir = lib.mkOption {
      type = lib.types.str;
      default = "/var/lib/narc/images";
      description = "Per-node base image cache, exposed to runner jobs as host volume narc-images.";
    };
    cniConfigDir = lib.mkOption {
      type = lib.types.str;
      default = "/opt/cni/config";
      description = "The Nomad client's cni_config_dir.";
    };
  };

  config = lib.mkIf cfg.enable {
    # The qemu driver is detected when qemu-system-* is on the agent's PATH.
    services.nomad.extraPackages = [ pkgs.qemu_kvm ];

    services.nomad.settings.client = {
      cni_path = lib.mkDefault "${pkgs.cni-plugins}/bin";
      cni_config_dir = lib.mkDefault cfg.cniConfigDir;
      host_volume.narc-images = {
        path = cfg.cacheDir;
        read_only = false;
      };
    };

    systemd.tmpfiles.rules = [
      "d ${cfg.cacheDir} 0755 root root -"
      "d ${cfg.cniConfigDir} 0755 root root -"
      "L+ ${cfg.cniConfigDir}/narc.conflist - - - - ${conflist}"
    ];

    networking.nftables.tables.narc = {
      family = "inet";
      # Reuse the shipped ruleset's body, filling in the node addresses.
      content = lib.pipe nft [
        (s: lib.head (builtins.match ".*table inet narc \\{(.*)}[[:space:]]*" s))
        (lib.replaceStrings [ "# elements = { 203.0.113.1, 203.0.113.2, 203.0.113.3 }" ]
          [ (lib.optionalString (cfg.nodeAddresses != [ ]) "elements = { ${addrs} }") ])
      ];
    };
  };
}
