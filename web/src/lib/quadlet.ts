// Keys per section from podman-systemd.unit(5), used for editor completion
const common = ['ContainersConfModule', 'GlobalArgs', 'PodmanArgs', 'ServiceName']

export const sections: Record<string, string[]> = {
  Container: [
    ...common, 'AddCapability', 'AddDevice', 'AddHost', 'Annotation', 'AutoUpdate', 'CgroupsMode', 'ContainerName',
    'DNS', 'DNSOption', 'DNSSearch', 'DropCapability', 'Entrypoint', 'Environment', 'EnvironmentFile', 'EnvironmentHost',
    'Exec', 'ExposeHostPort', 'GIDMap', 'Group', 'GroupAdd', 'HealthCmd', 'HealthInterval', 'HealthLogDestination',
    'HealthMaxLogCount', 'HealthMaxLogSize', 'HealthOnFailure', 'HealthRetries', 'HealthStartPeriod', 'HealthStartupCmd',
    'HealthStartupInterval', 'HealthStartupRetries', 'HealthStartupSuccess', 'HealthStartupTimeout', 'HealthTimeout',
    'HostName', 'Image', 'IP', 'IP6', 'Label', 'LogDriver', 'LogOpt', 'Mask', 'Memory', 'Mount', 'Network', 'NetworkAlias',
    'NoNewPrivileges', 'Notify', 'PidsLimit', 'Pod', 'PublishPort', 'Pull', 'ReadOnly', 'ReadOnlyTmpfs', 'ReloadCmd',
    'ReloadSignal', 'Retry', 'RetryDelay', 'Rootfs', 'RunInit', 'SeccompProfile', 'Secret', 'SecurityLabelDisable',
    'SecurityLabelFileType', 'SecurityLabelLevel', 'SecurityLabelNested', 'SecurityLabelType', 'ShmSize', 'StartWithPod',
    'StopSignal', 'StopTimeout', 'SubGIDMap', 'SubUIDMap', 'Sysctl', 'Timezone', 'Tmpfs', 'UIDMap', 'Ulimit', 'Unmask',
    'User', 'UserNS', 'Volume', 'WorkingDir',
  ],
  Pod: [
    ...common, 'AddHost', 'DNS', 'DNSOption', 'DNSSearch', 'ExitPolicy', 'GIDMap', 'HostName', 'IP', 'IP6', 'Label',
    'Network', 'NetworkAlias', 'PodName', 'PublishPort', 'ShmSize', 'StopTimeout', 'SubGIDMap', 'SubUIDMap', 'UIDMap',
    'UserNS', 'Volume',
  ],
  Network: [
    ...common, 'DisableDNS', 'DNS', 'Driver', 'Gateway', 'InterfaceName', 'Internal', 'IPAMDriver', 'IPRange', 'IPv6',
    'Label', 'NetworkDeleteOnStop', 'NetworkName', 'Options', 'Subnet',
  ],
  Volume: [
    ...common, 'Copy', 'Device', 'Driver', 'Group', 'Image', 'Label', 'Options', 'Type', 'User', 'VolumeName',
  ],
  Kube: [
    ...common, 'AutoUpdate', 'ConfigMap', 'ExitCodePropagation', 'KubeDownForce', 'LogDriver', 'Network', 'PublishPort',
    'SetWorkingDirectory', 'UserNS', 'Yaml',
  ],
  Image: [
    ...common, 'AllTags', 'Arch', 'AuthFile', 'CertDir', 'Creds', 'DecryptionKey', 'Image', 'ImageTag', 'OS',
    'Policy', 'Retry', 'RetryDelay', 'TLSVerify', 'Variant',
  ],
  Build: [
    ...common, 'Annotation', 'Arch', 'AuthFile', 'BuildArg', 'Environment', 'File', 'ForceRM', 'GroupAdd', 'IgnoreFile',
    'ImageTag', 'Label', 'Network', 'Pull', 'Retry', 'RetryDelay', 'Secret', 'SetWorkingDirectory', 'Target',
    'TLSVerify', 'Variant', 'Volume',
  ],
  Unit: [
    'Description', 'Documentation', 'Wants', 'Requires', 'Requisite', 'BindsTo', 'PartOf', 'Upholds', 'Conflicts',
    'Before', 'After', 'OnFailure', 'OnSuccess', 'StartLimitIntervalSec', 'StartLimitBurst', 'ConditionPathExists',
    'AssertPathExists', 'RequiresMountsFor',
  ],
  Service: [
    'Restart', 'RestartSec', 'TimeoutStartSec', 'TimeoutStopSec', 'ExecStartPre', 'ExecStartPost', 'ExecStopPost',
    'Environment', 'EnvironmentFile', 'Slice', 'Nice', 'CPUQuota', 'MemoryMax', 'MemoryHigh', 'SuccessExitStatus',
  ],
  Install: ['WantedBy', 'RequiredBy', 'Alias', 'Also'],
  Quadlet: ['DefaultDependencies'],
}

export const kindSection: Record<string, string> = {
  container: 'Container', pod: 'Pod', network: 'Network', volume: 'Volume', kube: 'Kube', image: 'Image', build: 'Build',
}

export const kindsForNew = ['container', 'pod', 'network', 'volume', 'image', 'build', 'kube', 'env'] as const

export function template(kind: string, name: string, inPod?: string): string {
  switch (kind) {
    case 'container':
      return `[Unit]
Description=${name}

[Container]
Image=docker.io/library/${name}:latest
ContainerName=${name}
${inPod ? `Pod=${inPod}\n` : 'PublishPort=8080:80\n'}Volume=%h/appdata/${name}:/config:Z
Environment=TZ=Europe/Berlin
AutoUpdate=registry

[Service]
Restart=always

[Install]
WantedBy=default.target
`
    case 'pod':
      return `[Unit]
Description=${name} pod

[Pod]
PodName=${name}
PublishPort=8080:80

[Install]
WantedBy=default.target
`
    case 'network':
      return `[Network]
NetworkName=${name}
`
    case 'volume':
      return `[Volume]
VolumeName=${name}
`
    case 'image':
      return `[Image]
Image=docker.io/library/${name}:latest
`
    case 'build':
      return `[Build]
ImageTag=localhost/${name}:latest
SetWorkingDirectory=unit
`
    case 'kube':
      return `[Kube]
Yaml=${name}.yaml

[Install]
WantedBy=default.target
`
    default:
      return '# KEY=value\n'
  }
}

// Turns systemd's unit state into the badge shown in the UI
export function unitBadge(active?: string): 'running' | 'failed' | 'stopped' | 'busy' {
  if (active === 'active') return 'running'
  if (active === 'failed') return 'failed'
  if (active === 'activating' || active === 'deactivating' || active === 'reloading') return 'busy'
  return 'stopped'
}
