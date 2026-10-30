# Local development with Tilt on OrbStack Kubernetes.
#
#   tilt up
#
# It builds the operator and datactl, installs the Helm chart in dist/chart, and starts a small Silo
# (S3 compatible storage, dev/silo.yaml) for snapshots. The sample servers and the snapshot are manual:
# trigger them in the Tilt UI in this order: golden-source, golden-seed, golden-snapshot, pr-env, pr-env-verify.
#
# OPERATOR_NAMESPACED=1 tilt up   installs the chart with a Role in vttest-dev and not a ClusterRole.


load('ext://namespace', 'namespace_create')

# Under GitHub Actions (GITHUB_ACTIONS=true) the golden-data workflow runs automatically, so that
# plain `tilt ci` exercises it end to end. Locally it stays manual: trigger it in the Tilt UI.
ci = os.getenv('GITHUB_ACTIONS') == 'true'
golden_trigger_mode = TRIGGER_MODE_AUTO if ci else TRIGGER_MODE_MANUAL

# --- code generation and binaries (cross compiled for linux/amd64) ---

local_resource(
    'generate',
    cmd='make manifests generate',
    deps=['api'],
    ignore=['api/v1alpha1/zz_generated.deepcopy.go'],
    labels=['build'],
)

local_resource(
    'compile-manager',
    cmd='CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/manager_linux ./cmd',
    deps=['api', 'cmd/main.go', 'internal', 'go.mod', 'go.sum'],
    ignore=['**/*_test.go'],
    resource_deps=['generate'],
    labels=['build'],
)

# datactl is not an image field of a Kubernetes object: the operator gets its name through a flag, so
# Tilt cannot build it with docker_build. Build it here. OrbStack shares its images with Kubernetes.
local_resource(
    'datactl-image',
    cmd='CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/datactl_linux ./cmd/datactl && ' +
        'docker build -q -t vttestserver-operator-datactl:dev -f dev/datactl.Dockerfile bin',
    deps=['cmd/datactl', 'internal/snapshot', 'go.mod', 'go.sum'],
    ignore=['**/*_test.go'],
    labels=['build'],
)

docker_build(
    'vttestserver-operator-manager:dev',
    'bin',
    dockerfile='dev/manager.Dockerfile',
    only=['manager_linux'],
)

# --- operator: the Helm chart ---

# Tilt does not see the files that helm() reads in the chart, so watch them.
watch_file('dist/chart')
namespace_create('vttestserver-operator-system')

chart_set = [
    'manager.image.repository=vttestserver-operator-manager',
    'manager.image.tag=dev',
    'datactl.image.repository=vttestserver-operator-datactl',
    'datactl.image.tag=dev',
    'manager.args[0]=--leader-elect',
    'manager.args[1]=--zap-devel',
    # Plain HTTP metrics: no token needed to look at them with curl.
    'metrics.secure=false',
]
manager_rbac = [
    'vttestserver-operator-manager-role:clusterrole',
    'vttestserver-operator-manager-rolebinding:clusterrolebinding',
]
if os.getenv('OPERATOR_NAMESPACED'):
    chart_set += ['rbac.namespaced=true', 'manager.watchNamespaces[0]=vttest-dev']
    manager_rbac = [
        'vttestserver-operator-manager-role:role:vttest-dev',
        'vttestserver-operator-manager-rolebinding:rolebinding:vttest-dev',
    ]

k8s_yaml(helm(
    'dist/chart',
    name='vttestserver-operator',
    namespace='vttestserver-operator-system',
    set=chart_set,
))
k8s_resource(
    'vttestserver-operator-controller-manager',
    new_name='operator',
    resource_deps=['compile-manager', 'datactl-image'],
    objects=[
        'vttestservers.vttestserver-operator.io:customresourcedefinition',
        'vttestserversnapshots.vttestserver-operator.io:customresourcedefinition',
        'vttestserver-operator-controller-manager:serviceaccount',
        'vttestserver-operator-leader-election-role:role',
        'vttestserver-operator-leader-election-rolebinding:rolebinding',
    ] + manager_rbac,
    labels=['operator'],
)

# --- Silo, a small S3 compatible store for snapshots ---

k8s_yaml('dev/silo.yaml')
k8s_resource(
    'silo',
    objects=['silo:namespace', 'silo:secret:silo', 'silo:persistentvolumeclaim:silo'],
    labels=['storage'],
    port_forwards=['19000:9000', '19001:9001'],
)
k8s_resource('silo-post-job', resource_deps=['silo'], labels=['storage'])

# --- sample resources: the golden data workflow ---

k8s_yaml(['dev/namespace.yaml', 'dev/s3-credentials.yaml', 'dev/vschema.yaml'])
k8s_resource(
    new_name='dev-config',
    objects=['vttest-dev:namespace', 's3-credentials:secret:vttest-dev',
             'commerce-vschema:configmap:vttest-dev', 'commerce-init-sql:configmap:vttest-dev'],
    resource_deps=['operator'],
    labels=['samples'],
)

k8s_yaml('dev/golden-source.yaml')
k8s_resource(
    new_name='golden-source',
    objects=['golden-source:vttestserver:vttest-dev'],
    resource_deps=['dev-config'],
    auto_init=ci,
    trigger_mode=golden_trigger_mode,
    labels=['samples'],
)

k8s_yaml('dev/seed-job.yaml')
k8s_resource(
    'golden-seed',
    resource_deps=['golden-source'],
    auto_init=ci,
    trigger_mode=golden_trigger_mode,
    labels=['samples'],
)

k8s_yaml('dev/snapshot.yaml')
k8s_resource(
    new_name='golden-snapshot',
    objects=['golden-1:vttestserversnapshot:vttest-dev'],
    resource_deps=['golden-seed', 'silo'],
    auto_init=ci,
    trigger_mode=golden_trigger_mode,
    labels=['samples'],
)

k8s_yaml('dev/pr-env.yaml')
k8s_resource(
    new_name='pr-env',
    objects=['pr-env:vttestserver:vttest-dev'],
    resource_deps=['golden-snapshot'],
    auto_init=ci,
    trigger_mode=golden_trigger_mode,
    labels=['samples'],
)

k8s_yaml('dev/pr-env-verify-job.yaml')
k8s_resource(
    'pr-env-verify',
    resource_deps=['pr-env'],
    auto_init=ci,
    trigger_mode=golden_trigger_mode,
    labels=['samples'],
)

k8s_yaml('dev/mysql-config-test.yaml')
k8s_resource(
    'cfg-fresh-verify',
    resource_deps=['dev-config'],
    objects=['cfg-fresh:vttestserver:vttest-dev'],
    auto_init=False,
    trigger_mode=TRIGGER_MODE_MANUAL,
    labels=['tests'],
)

k8s_yaml('dev/s3-delete-test.yaml')
for name in ['snap-keep', 'snap-delete', 'snap-ttl']:
    k8s_resource(
        new_name=name,
        objects=[name + ':vttestserversnapshot:vttest-dev'],
        resource_deps=['golden-source', 'silo'],
        auto_init=False,
        trigger_mode=TRIGGER_MODE_MANUAL,
        labels=['tests'],
    )
k8s_resource(
    's3-list',
    resource_deps=['silo'],
    auto_init=False,
    trigger_mode=TRIGGER_MODE_MANUAL,
    labels=['tests'],
)
