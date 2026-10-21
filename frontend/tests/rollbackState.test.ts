import test from "node:test";
import assert from "node:assert/strict";
import { rollbackModalStateKey } from "../src/pages/rollbackState.js";

test("TestRollbackModalResetsOnSnapshotChange", () => {
  const first = rollbackModalStateKey(true, "snapshot-a");
  assert.notEqual(rollbackModalStateKey(true, "snapshot-b"), first);
  assert.notEqual(rollbackModalStateKey(false, "snapshot-a"), first);
  assert.notEqual(rollbackModalStateKey(true, "snapshot-a"), rollbackModalStateKey(false, null));
});
