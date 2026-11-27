# Samples of the objects a FactorioServer refers to.

A `FactorioServer` refers to three objects that you create yourself:

| Object | File | Used for |
| --- | --- | --- |
| PersistentVolumeClaim | [`pvc.yaml`](pvc.yaml) | the save data (`.spec.savePersistentVolumeClaimName`) |
| ConfigMap | [`configmap.yaml`](configmap.yaml) | `config.ini` and the server whitelist (`.spec.gameConfig`) |
| Secret | [`secret.yaml`](secret.yaml) | the Discord webhook URL, the S3 credentials and, for experimental or old builds, the Factorio download credentials |

Apply them in this order — the PVC must exist before the FactorioServer is
created, because the controller never creates or deletes it:

```sh
kubectl apply -n <namespace> -f examples/pvc.yaml
kubectl apply -n <namespace> -f examples/configmap.yaml -f examples/secret.yaml
kubectl apply -n <namespace> -f examples/factorioserver.yaml
```

`secret.yaml` holds placeholders: substitute real values in what you apply,
never in the tracked file. If your cluster has no default StorageClass, set
`storageClassName` in `pvc.yaml`.

The operator itself is installed with the Helm chart of this repository; see
the quick start in [README.md](../README.md).
