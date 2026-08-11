# Stateless

**State on S3, not on metal.** A HopOS node owns nothing:

- The boot medium carries a **signed image and six lines of config** —
  that is the entire local footprint. There is no disk install, no package
  state, no config drift, nothing to back up.
- **Desired state lives in S3.** The leader commits the cluster's jobs as
  one object; leader election runs on a CAS lock in the same bucket.
- **Images are fetched, never stored.** A job start pulls its image from the
  artifact URL and streams it straight into the slot's partition, where it
  runs; the node keeps no copy. Even shared data is designed to be *fetched,
  not owned*.

## What that buys you

```mermaid
flowchart TD
  P["power cut — 126 jobs running"] --> O["power back on"]
  O --> D["discover hardware (ACPI / DTB)"]
  D --> S["load committed state from S3"]
  S --> F["each job's image streamed into its slot"]
  F --> R["126/126 running — 17.4 s, zero resubmits"]
```

- **Reboot recovery is not a feature, it's the boot path.** Every boot is
  identical: discover hardware → join cluster → load committed state →
  stream every job's image into its slot. A power cut just exercises it.
- **"Reinstall" means re-imaging a USB stick.** A broken node is a stick
  swap; a lost machine is the same stick in a different box.
- **Updates are a file copy.** New image on the stick (or served over
  signed HTTP), reboot — the state comes back by itself. The
  [imager](../boot.md#the-imager--burn-configure-find) does the same to a
  card and keeps its config, so an update is new code on the same node.
- **Nothing to steal.** A machine walking out of the rack carries no data
  and no state — only the boot stick matters (see the
  [trust model](../config.md#trust-model)).
