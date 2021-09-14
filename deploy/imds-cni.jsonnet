// Modify as necessary.

local mtu = 9001;

{
  cniVersion: "0.3.1",
  name: "aws-vpc",
  plugins: [
    {
      type: "imds-ptp",
      mtu: mtu,
      ipam: {
        type: "imds-ipam",
        routes: [{dst: "0.0.0.0/0"}],
        dataDir: "/run/cni/ipam",
      },
    },
    {
      type: "portmap",
      capabilities: {portMappings: true},
    },
    {
      type: "bandwidth",
      capabilities: {bandwidth: true},
    },
  ],
}
