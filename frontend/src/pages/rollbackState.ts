export function rollbackModalStateKey(open: boolean, snapshotId?: string | null): string {
  return open && snapshotId ? `snapshot:${snapshotId}` : "closed";
}
