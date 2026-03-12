# Snapshots, Restore, and Fork

Cleanroom supports capturing sandbox filesystem state as immutable snapshots.
Snapshots can be used to fork new sandboxes or restore an existing sandbox to a
previous state. Every restore and fork boots a fresh VM from the captured disk
state — no process state survives.

## Prerequisites

Snapshot support requires a configured volume driver. Currently only the ZFS
driver is supported on the Firecracker backend.

### ZFS setup

1. Create a ZFS dataset for Cleanroom:

   ```bash
   sudo zpool create tank /dev/sdX          # or use an existing pool
   sudo zfs create tank/cleanroom
   ```

2. Grant the Cleanroom user access (using sudo or the privileged helper):

   ```sudoers
   Cmnd_Alias CLEANROOM_ZFS = /usr/sbin/zfs *, /usr/bin/dd *
   CLEANROOM_CI ALL=(root) NOPASSWD: CLEANROOM_ZFS
   ```

3. Enable snapshots in runtime config (`~/.config/cleanroom/config.yaml`):

   ```yaml
   backends:
     firecracker:
       snapshots:
         enabled: true
         driver: zfs
         zfs_dataset: tank/cleanroom
         quiesce_timeout_seconds: 15
   ```

4. Verify with `cleanroom doctor`:

   ```bash
   cleanroom doctor
   ```

   Look for passing `snapshot_driver`, `snapshot_zfs_dataset`, and
   `snapshot_zfs_dataset_access` checks.

## CLI Usage

### Create a snapshot

Capture the current filesystem state of a sandbox:

```bash
cleanroom snapshot create <sandbox-id> --name "after-deps"
```

The sandbox must be idle (no active execution or file download).

### List snapshots

```bash
cleanroom snapshot ls
cleanroom snapshot ls --json
```

### Get snapshot details

```bash
cleanroom snapshot get <snapshot-id>
cleanroom snapshot get <snapshot-id> --json
```

### Delete a snapshot

```bash
cleanroom snapshot rm <snapshot-id>
```

### Fork a new sandbox from a snapshot

```bash
cleanroom sandbox create --from-snapshot <snapshot-id>
```

The new sandbox inherits the snapshot's policy and backend. A fresh VM boots
from a copy-on-write clone of the snapshot.

### Restore a sandbox to a snapshot

```bash
cleanroom sandbox restore <sandbox-id> --snapshot <snapshot-id>
```

The sandbox's current volume is replaced with a fresh clone of the snapshot and
a new VM boots. The sandbox keeps its ID but all process state is discarded.

## CI Bootstrap and Fan-Out

A common CI pattern uses snapshots to avoid repeating expensive setup work:

```bash
# 1. Create a sandbox
SB=$(cleanroom sandbox create --json | jq -r .sandbox.sandbox_id)

# 2. Run bootstrap work
cleanroom exec $SB -- git clone https://github.com/org/repo /work
cleanroom exec $SB -- sh -c 'cd /work && npm ci'

# 3. Snapshot the golden state
SNAP=$(cleanroom snapshot create $SB --name deps --json | jq -r .snapshot.snapshot_id)

# 4. Fan out parallel test shards from the snapshot
for shard in unit integration e2e; do
  FORK=$(cleanroom sandbox create --from-snapshot $SNAP --json | jq -r .sandbox.sandbox_id)
  cleanroom exec $FORK -- sh -c "cd /work && npm test -- --shard=$shard" &
done
wait

# 5. Clean up
cleanroom snapshot rm $SNAP
cleanroom sandbox terminate $SB
```

Each forked sandbox gets its own VM, network identity, and writable volume.
Changes in one fork do not affect others or the snapshot.

### Guest-initiated checkpoints

The guest agent supports requesting a checkpoint from inside a running command.
This is useful when the workload knows the exact safe point:

```bash
# Inside the guest, after setup work completes:
cleanroom-guest-agent checkpoint request --name "ready"
```

The host records the request and materializes the snapshot once the current
execution finishes and the sandbox becomes idle. The host remains authoritative
— the guest request is advisory.

## Snapshot Lineage

Snapshots support lineage: a forked sandbox can create further snapshots,
forming a tree. Each snapshot records its source sandbox ID. The parent
relationship is tracked implicitly through sandbox provenance.

## How It Works

1. **Create snapshot:** The host asks the guest agent to quiesce (sync
   filesystems), pauses the VM via the Firecracker API, takes a ZFS snapshot of
   the sandbox volume, then resumes the VM.

2. **Fork:** ZFS clones the snapshot into a new writable volume. A fresh VM
   boots from the clone with new network identity.

3. **Restore:** The current VM is terminated, the writable volume is destroyed,
   a fresh ZFS clone is created from the target snapshot, and a new VM boots.

4. **Delete:** The ZFS snapshot is destroyed and metadata is removed from the
   SQLite store.

## Related

- [backend/firecracker.md](backend/firecracker.md) — Firecracker backend details
- [plans/snapshot-restore-fork.md](plans/snapshot-restore-fork.md) — full design document
- [ci.md](ci.md) — CI setup guide
