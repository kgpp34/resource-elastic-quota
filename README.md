# resource-elastic-quota

面向多部门、异构节点池的 Kubernetes 弹性内存 Limit 配额 Controller。

英文架构与实施设计见 [`docs/design-and-implementation.md`](docs/design-and-implementation.md)。

## Description

Controller 按 `department + ResourcePool` 统计业务 Pod 的 `limits.memory`，根据 base/max quota、权重和资源池超卖比例动态计算 effective quota。Validating Admission Webhook 对 Pod 做最终准入，并对 Deployment、StatefulSet、DaemonSet、ReplicaSet、Job 和 CronJob 提供提前检查。requests 与 CPU 不参与本项目的额度统计或拒绝判断。

## Getting Started

### Prerequisites
- Go 1.24+（当前本地生成工具要求；运行依赖保持 Kubernetes 1.23 版本线）
- Docker 与 kubectl
- Kubernetes 1.23+
- cert-manager（默认部署使用它签发并注入 Webhook TLS 证书）
- 生产环境至少两个可调度节点，以运行双副本 Webhook

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/resource-elastic-quota:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/resource-elastic-quota:tag
```

默认清单会创建 Webhook Service、ValidatingWebhookConfiguration、Certificate、Issuer、双副本 Deployment 和 PodDisruptionBudget。必须先确保 cert-manager 已就绪，否则 Webhook 证书和 CA bundle 不会生成。

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Project Distribution

Following the options to release and provide this solution to the users.

### By providing a bundle with all YAML files

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/resource-elastic-quota:tag
```

**NOTE:** The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without its
dependencies.

2. Using the installer

Users can just run 'kubectl apply -f <URL for YAML BUNDLE>' to install
the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/resource-elastic-quota/<tag or branch>/dist/install.yaml
```

### By providing a Helm Chart

1. Build the chart using the optional helm plugin

```sh
kubebuilder edit --plugins=helm/v1-alpha
```

2. See that a chart was generated under 'dist/chart', and users
can obtain this solution from there.

**NOTE:** If you change the project, you need to update the Helm Chart
using the same command above to sync the latest changes. Furthermore,
if you create webhooks, you need to use the above command with
the '--force' flag and manually ensure that any custom configuration
previously added to 'dist/chart/values.yaml' or 'dist/chart/manager/manager.yaml'
is manually re-applied afterwards.

## Contributing
// TODO(user): Add detailed information on how you would like others to contribute to this project

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright kgpp34 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
