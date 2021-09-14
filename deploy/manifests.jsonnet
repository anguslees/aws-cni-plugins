// Execute me with:
//  jsonnet -S -m $outdir manifest.jsonnet

local objectValues(obj) = [obj[k] for k in std.objectFields(obj)];
local namedList(obj) = [{name: k} + obj[k] for k in std.objectFields(obj)];

local image = "ghcr.io/anguslees/aws-cni-plugins:latest";

local pauseDaemonset = {
  daemonset: {
    local name = self.metadata.name,

    kind: "DaemonSet",
    apiVersion: "apps/v1",
    metadata: {
      // NB: chosen to replace existing aws-node daemonset. You may want
      // to choose another name.
      name: "aws-node",
      namespace: "kube-system",

      labels: {"k8s-app": name},
    },
    spec: {
      local spec = self,
      updateStrategy: {
        type: "RollingUpdate",
        rollingUpdate: {maxUnavailable: "10%"},
      },
      selector: {
        matchLabels: spec.template.metadata.labels,
      },
      template: {
        metadata: {
          labels: {"k8s-app": name},
        },
        spec: {
          priorityClassName: "system-node-critical",
          terminationGracePeriodSeconds: 1,
          hostNetwork: true,
          automountServiceAccountToken: false,
          tolerations: [{
            effect: "NoSchedule",
            operator: "Exists",
            key: "node.kubernetes.io/not-ready",
          }],

          affinity: {
            nodeAffinity: {
              requiredDuringSchedulingIgnoredDuringExecution: {
                nodeSelectorTerms: [{
                  matchExpressions: [
                    {
                      key: "kubernetes.io/os",
                      operator: "In",
                      values: ["linux"],
                    },
                    {
                      key: "kubernetes.io/arch",
                      operator: "In",
                      values: ["amd64", "arm64"],
                    },
                    {
                      key: "eks.amazonaws.com/compute-type",
                      operator: "NotIn",
                      values: ["fargate"],
                    },
                  ],
                }],
              },
            },
          },

          volumes_:: {},
          volumes: namedList(self.volumes_),

          initContainers_:: {},
          initContainers: std.sort(
            namedList(self.initContainers_),
            keyF=function(x) ({order:: 50} + x).order,
          ),

          containers_:: {
            pause: {
              image: "public.ecr.aws/eks-distro/kubernetes/pause:3.2",
            },
          },
          containers: namedList(self.containers_),
        },
      },
    },
  },
};

local imdsCni = pauseDaemonset {
  daemonset+: {
    spec+: {
      template+: {
        spec+: {

          volumes_+: {
            "cni-bin-dir": {
              hostPath: {path: "/opt/cni/bin", type: "DirectoryOrCreate"},
            },
            "cni-net-dir": {
              hostPath: {path: "/etc/cni/net.d", type: "DirectoryOrCreate"},
            },
          },

          initContainers_+: {
            install: {
              image: image,
              local configTemplate = importstr "imds-cni.jsonnet",
              command: ["/bin/sh", "-x", "-e", "-c", self.shcmd],
              shcmd:: |||
                cd /usr/local/bin
                attach-enis --max-ips=250
                install -m755 imds-ipam imds-ptp bandwidth portmap /opt/cni/bin/
                json-tmpl --file=- -v=4 --logtostderr >/etc/cni/net.d/10-aws.conflist <<'EOF'
                %sEOF
              ||| % configTemplate,
              volumeMounts: [
                {name: "cni-bin-dir", mountPath: "/opt/cni/bin"},
                {name: "cni-net-dir", mountPath: "/etc/cni/net.d"},
              ],
            },
          },
        },
      },
    },
  },
};

local v6pdCni = pauseDaemonset {
  daemonset+: {
    spec+: {
      template+: {
        spec+: {

          volumes_+: {
            "cni-bin-dir": {
              hostPath: {path: "/opt/cni/bin", type: "DirectoryOrCreate"},
            },
            "cni-net-dir": {
              hostPath: {path: "/etc/cni/net.d", type: "DirectoryOrCreate"},
            },
          },

          initContainers_+: {
            attach: {
              // NB: this step is unnecessary if we can rely on the PD
              // attach happening in the LaunchTemplate (or similar).
              image: "public.ecr.aws/bitnami/aws-cli:2.2.37",
              order:: 20, // sort before 'install'
              command: ["/bin/sh", "-x", "-e", "-c", self.shcmd],
              shcmd:: |||
                token=$(curl -q --retry 5 --fail -X PUT -H "X-aws-ec2-metadata-token-ttl-seconds: 600" http://169.254.169.254/latest/api/token)
                metadata() {
                  curl -q --retry 5 --fail -H "X-aws-ec2-metadata-token: $token" http://169.254.169.254/2019-10-01/meta-data/$1
                }

                mac=$(metadata mac)
                if [ -n "$(metadata network/interfaces/macs/$mac/ipv6-prefix)" ]; then
                  exit 0
                fi

                ifid=$(metadata network/interfaces/macs/$mac/interface-id)
                aws ec2 assign-ipv6-addresses --network-interface-id $ifid --ipv6-prefix-count 1
                while [ -z "$(metadata network/interfaces/macs/$mac/ipv6-prefix)" ]; do
                  sleep 1
                done
              |||,
            },
            install: {
              image: image,
              local configTemplate = importstr "v6pd.jsonnet",
              command: ["/bin/sh", "-x", "-e", "-c", self.shcmd],
              shcmd:: |||
                cd /usr/local/bin
                install -m755 host-local bandwidth portmap ptp egress-v4 /opt/cni/bin/
                json-tmpl --file=- -v=4 --logtostderr >/etc/cni/net.d/10-aws.conflist <<'EOF'
                %sEOF
              ||| % configTemplate,
              volumeMounts: [
                {name: "cni-bin-dir", mountPath: "/opt/cni/bin"},
                {name: "cni-net-dir", mountPath: "/etc/cni/net.d"},
              ],
            },
          },
        },
      },
    },
  },
};

local output = {
  "imds-cni": imdsCni,
  "v6pd-cni": v6pdCni,
};

// Yaml-ified output files
{
  [k + ".yaml"]: std.manifestYamlStream(objectValues(output[k]))
  for k in std.objectFields(output)
}
