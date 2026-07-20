# GCP Spot Auto-Start · Step-by-Step Guide

> Expanded version of the [GCP Spot auto-start](../README_EN.md#gcp-spot-auto-start-optional) section in the README: every step, every field, how to verify, plus an FAQ.

GCP Spot instances are cheap (typically 60–90% off on-demand), but can be preempted at any time — the instance goes `TERMINATED` (disk data is kept), and it will not come back on its own. Moss acts as a watchdog: once a node is confirmed offline, it calls the Compute Engine API to start the instance again, with every step and outcome pushed via Telegram.

```
node offline ──(wait "confirmation delay", default 120s)──▶ query instance status
                                          ├─ TERMINATED ──▶ call start ──on failure──▶ retry after cooldown, give up + notify past the attempt cap
                                          └─ RUNNING    ──▶ leave the instance alone, alert only (agent / network problem)
node back online ──▶ retry counter resets
```

## 0. Before you start

- Moss **v1.1.0+**, panel deployed and receiving node reports;
- **The panel must NOT run on a guarded Spot instance** — if they get preempted together, nothing is left to restart anything;
- Keep the Spot instance's termination action at the default **STOP**. With **DELETE**, the instance is gone after preemption and no tool can bring it back;
- Set up Telegram notifications first (admin "通知" / Notifications tab) — success / failure / give-up all arrive there.

## 1. Create a least-privilege Service Account

The guardian only needs "get instance status" and "start instance". Create a dedicated minimal account — even if leaked, it can't do much damage.

Open the [GCP Console](https://console.cloud.google.com/), click the **Cloud Shell** icon (`>_`) in the top-right corner, and paste the whole block (only change the first line to your project ID):

```bash
PROJECT=your-project-id

# 1) dedicated Service Account
gcloud iam service-accounts create moss-starter --project=$PROJECT

# 2) custom role with exactly two permissions
gcloud iam roles create mossSpotStarter --project=$PROJECT \
  --permissions=compute.instances.get,compute.instances.start

# 3) bind the role to the account
gcloud projects add-iam-policy-binding $PROJECT \
  --member="serviceAccount:moss-starter@$PROJECT.iam.gserviceaccount.com" \
  --role="projects/$PROJECT/roles/mossSpotStarter"

# 4) generate the key file
gcloud iam service-accounts keys create moss-sa.json \
  --iam-account=moss-starter@$PROJECT.iam.gserviceaccount.com
```

You should see `created key [...] as [moss-sa.json]`. Then:

```bash
cat moss-sa.json
```

Copy the **entire JSON** (from `{` to `}`) — you'll paste it into the panel next. Once pasted, delete the file:

```bash
rm moss-sa.json
```

> Shortcut: the predefined role `roles/compute.instanceAdmin.v1` also works, but it can delete and reconfigure instances too — far broader than needed, not recommended.

## 2. Panel global setup ("GCP 守护" / GCP Guardian tab)

In the Moss admin panel → **GCP Guardian**:

1. Paste the JSON from the previous step into the credential box;
2. Click **Save & test connection** — the panel performs a real token exchange; on success it shows the account email and project ID;
3. Enable the **auto-start master switch**.

The three parameters are fine at their defaults:

| Parameter | Default | Meaning |
| --- | --- | --- |
| Confirmation delay | 120s | how long a node must stay offline before the instance status is checked — avoids false starts on network blips |
| Cooldown | 300s | wait between retries after a failed start (common when Spot capacity is short) |
| Max attempts | 3 | give up and notify after this many consecutive failures; resets automatically once the node comes back online |

## 3. Per-node setup ("服务器" / Servers tab, edit dialog)

The credential is global; each guarded node carries its own location info. On the **Servers** tab, edit the node:

1. Check **GCP auto-start**;
2. **Zone**: the instance's zone, e.g. `asia-east2-a`. Find it in the "Zone" column of "Compute Engine → VM instances" — note it's a zone (with the `-a`/`-b` suffix), not a region;
3. **Instance name**: the name in the **first column** of the GCP instance list, character-for-character. This is the GCP instance name, not the Moss node name;
4. **Project ID**: leave empty to use the credential's `project_id` — correct in almost all cases (see cross-project below).

Repeat for every Spot instance. Instances in the same project share the one credential — nothing extra on the GCP side.

## 4. Verify

After saving, a **▶** manual-start button appears on the node row. Click it:

- Instance running → "already running, nothing to do" — **that's the answer you want**: credential, zone and instance name are all correct end to end;
- Instance stopped → it actually starts; the node should come back online within a minute or two;
- Error → see [Troubleshooting](#troubleshooting) below.

The ▶ button works regardless of the master switch; automatic guarding requires both the master switch and the node switch to be on.

## Cross-project guarding

Instances spread across multiple projects don't need another credential — grant the same Service Account access in the other project. In Cloud Shell (`OTHER` = the other project ID, `SA_PROJECT` = the project the credential belongs to):

```bash
OTHER=other-project-id
SA_PROJECT=credential-project-id
gcloud iam roles create mossSpotStarter --project=$OTHER \
  --permissions=compute.instances.get,compute.instances.start
gcloud projects add-iam-policy-binding $OTHER \
  --member="serviceAccount:moss-starter@$SA_PROJECT.iam.gserviceaccount.com" \
  --role="projects/$OTHER/roles/mossSpotStarter"
```

Then fill the node's "Project ID" field with the `OTHER` project ID.

## FAQ

**I need to shut a machine down for maintenance — how do I avoid it being pulled back up?**
Turn off that node's "GCP auto-start" switch (or the master switch) first, then shut down. Remember to re-enable afterwards.

**I'm changing the project's billing account — do I need to reconfigure anything?**
No. Billing only affects charging; it has nothing to do with IAM or the credential. One caveat: don't leave the project without a valid billing account mid-switch — that disables the Compute API and stops instances, and the guardian can't fix a billing-suspended project (starts will just keep failing).

**The instance is RUNNING but the panel says the node is offline?**
That's not a GCP problem — the agent died or the network is down. Moss deliberately alerts without touching the instance in this case: blindly restarting a machine that's serving traffic is worse than doing nothing. Check the agent service on the machine.

**Starts keep failing until it gives up — what's the usual cause?**
Most often Spot capacity shortage in that zone (error contains `ZONE_RESOURCE_POOL_EXHAUSTED`) — retry manually with ▶ later, or consider rebuilding in another zone. It can also be a billing problem (billing account invalid / suspended). The notification includes GCP's error message.

**The instance is `SUSPENDED` — will it auto-resume?**
Not yet; only `TERMINATED` is handled. Resume suspended instances manually in the GCP console.

**Is the credential safe?**
It's stored in plaintext in the panel's SQLite database (a deliberate trade-off for the single-admin scenario). So: grant only the two minimal permissions, bind only the projects you need, and use a strong panel admin password. To revoke, delete the key (or the whole Service Account) in GCP Console "IAM → Service Accounts", then paste a fresh one into the panel.

**AWS / Azure / other clouds?**
Not yet — GCP only for now.

## Troubleshooting

| Symptom | Cause & fix |
| --- | --- |
| Save & test connection fails | incomplete JSON paste (must be `{` through `}`), or the Service Account / key was deleted — regenerate the key and paste again |
| ▶ returns 404 | wrong zone or instance name; a wrong project ID also 404s — check each field against the GCP instance list |
| ▶ returns 403 | missing permission: the role isn't bound, or a cross-project node lacks the cross-project grant above |
| Preempted but never auto-started | check in order: master switch on? → node switch on? → offline longer than the confirmation delay? → any "gave up" notification (attempts exhausted; no more retries until the node comes back — use ▶ manually) |
| Instance vanished after preemption | termination action was set to DELETE — rebuild the instance and keep STOP this time |
