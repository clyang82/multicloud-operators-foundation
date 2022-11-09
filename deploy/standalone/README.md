# standalone 

1. Install manifests on the standalone cluster

```bash
kubectl apply -k deploy/standalone/hub --kubeconfig=<standalone-kubeconfig>
```
2. Install manifests on the hosting cluster

```bash
cd deploy/standalone/manager && kustomize edit set namespace <the ns of standalone on hosting cluster> 

kubectl apply -k deploy/standalone/manager --kubeconfig=<hosting-kubeconfig>
```