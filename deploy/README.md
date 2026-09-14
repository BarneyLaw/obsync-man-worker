# Deployment

`deploy/` is the bootstrap template for
[BarneyLaw/homelab-cicd-config](https://github.com/BarneyLaw/homelab-cicd-config),
laid out exactly as it lands there:

| Here | In homelab-cicd-config | What |
|---|---|---|
| `deploy/apps/garage-obsync/` | `apps/garage-obsync/` | 3-node Garage StatefulSet; owns the `obsync` namespace |
| `deploy/argocd/garage-obsync.yaml` | `argocd/garage-obsync.yaml` | its Argo CD Application |
| `deploy/apps/obsync-worker/` | `apps/obsync-worker/` | the worker CronJob, rules, alerts |
| `deploy/argocd/obsync-worker.yaml` | `argocd/obsync-worker.yaml` | its Argo CD Application |

## What CI does

The `deploy` job in `.github/workflows/worker.yml` runs on `main` (push or
manual dispatch) after the Go tests, the Garage integration tests and the image
smoke test pass, the same contract as lag-app:

1. Build and push `ghcr.io/barneylaw/obsync-worker:<short-sha>` and `:latest`.
2. Check out homelab-cicd-config and copy in any of the files above that are
   **absent**. Nothing that already exists there is overwritten.
3. Set `apps/obsync-worker/kustomization.yaml` to the new tag.
4. Render both apps (a placeholder stands in for any SealedSecret not yet
   generated) and check the worker uses the new tag.
5. Push a `deploy: obsync-worker <sha>` commit. Argo CD syncs it.

After the first deploy, **homelab-cicd-config is the source of truth.** Change
the schedule, the worker rules or Garage's config there; editing them here only
affects a future bootstrap into an empty repo.

## One-time setup

1. **Token.** Add `CONFIG_REPO_TOKEN` as an Actions secret on
   obsync-man-worker: a fine-grained token with Contents read/write on
   homelab-cicd-config. The one lag-app uses has exactly that access.
2. **First deploy.** Push to `main` (or run the workflow by hand). The GitOps
   commit adds all four paths.
3. **Package visibility.** The first push creates the `obsync-worker` GHCR
   package as private, and nothing in the cluster has pull credentials. Set it
   public under the package's settings, as for lag-app.
4. **Garage.** In homelab-cicd-config, follow `apps/garage-obsync/README.md`:
   seal its secret, apply the Application, connect the nodes, assign the
   layout, create the bucket and key.
5. **Worker.** Follow `apps/obsync-worker/README.md`: seal the Canvas token and
   Garage key, apply the Application, run it once by hand.

## Check the manifests locally

```bash
scripts/render-deploy.sh deploy/apps/garage-obsync deploy/apps/obsync-worker
```
