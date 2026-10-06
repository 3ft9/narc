# NixOS module for a Nomad client that runs narc runner VMs.
#
#   imports = [ narc.nixosModules.node ];
#   services.narc-node = {
#     enable = true;
#     nodeAddresses = [ "203.0.113.1" "203.0.113.2" "203.0.113.3" ];
#   };
#
# Works with either firewall. With networking.nftables.enable, the egress
# rules are one of its tables. Otherwise (the iptables-based
# networking.firewall that Docker and Tailscale expect), a oneshot unit loads
# them as a separate `inet narc` table, which coexists with iptables: a drop
# in any base chain is final, whatever the iptables chains say.
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
  addrs = lib.concatStringsSep ", " cfg.nodeAddresses;
  # The shipped ruleset, with the node addresses filled in.
  nft = lib.replaceStrings [ "# elements = { 203.0.113.1, 203.0.113.2, 203.0.113.3 }" ]
    [ (lib.optionalString (cfg.nodeAddresses != [ ]) "elements = { ${addrs} }") ]
    (builtins.readFile ../deploy/narc.nft);
  # Declaring the table first makes the delete safe on first load, so `nft -f`
  # replaces the table in one transaction.
  ruleset = pkgs.writeText "narc.nft" ''
    table inet narc
    delete table inet narc
    ${nft}
  '';
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

    # Keep dhcpcd off the bridge.
    networking.dhcpcd.denyInterfaces = lib.mkIf config.networking.dhcpcd.enable [ "narc" ];

    networking.nftables.tables.narc = lib.mkIf config.networking.nftables.enable {
      family = "inet";
      content = lib.head (builtins.match ".*table inet narc \\{(.*)}[[:space:]]*" nft);
    };

    # Loaded before Nomad, so no runner VM runs without it. A change is a
    # reload (one atomic transaction), never a stop and start, so the table
    # is never missing; for the same reason there's no ExecStop. Not a
    # Requires= of Nomad's, which would restart Nomad along with it.
    systemd.services.narc-egress = lib.mkIf (!config.networking.nftables.enable) {
      description = "nftables egress rules for narc runner VMs";
      wantedBy = [ "multi-user.target" ];
      before = [ "nomad.service" ];
      reloadIfChanged = true;
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = "${pkgs.nftables}/bin/nft -f ${ruleset}";
        ExecReload = "${pkgs.nftables}/bin/nft -f ${ruleset}";
      };
    };
  };
}
