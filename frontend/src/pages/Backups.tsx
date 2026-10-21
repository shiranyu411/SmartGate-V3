import { useState } from "react";
import { useBackupCreate, useBackupRestore } from "../lib/queries";
import type { Backup } from "../lib/types";
import { Card, SectionHeader, Eyebrow } from "../ui/Card";
import { Button } from "../ui/Button";
import { Field, Input } from "../ui/Field";
import { Modal } from "../ui/Modal";
import { sans } from "../ui/tokens";
import { ErrorText } from "../ui/ErrorText";

export function BackupsPage() {
  const create = useBackupCreate();
  const restore = useBackupRestore();
  const [lastBackup, setLastBackup] = useState<Backup | null>(null);
  const [restoreModal, setRestoreModal] = useState(false);
  const [error, setError] = useState<unknown>(null);

  async function handleCreate() {
    setError(null);
    try {
      const result = await create.mutateAsync({});
      setLastBackup(result);
    } catch (e) {
      setError(e);
    }
  }

  return (
    <div className="space-y-12 max-w-[1120px]">
      <SectionHeader
        eyebrow="备份 · Backups"
        title="创建与恢复完整归档"
        desc="备份会生成 tar.gz 归档文件，包含完整配置状态。恢复会覆盖当前配置并通过 commit lane 落地。"
        right={
          <div className="flex items-center gap-3">
            <Button onClick={handleCreate} loading={create.isPending}>
              立即备份
            </Button>
            <Button variant="ghost" onClick={() => setRestoreModal(true)}>
              恢复备份
            </Button>
          </div>
        }
      />

      {error ? (
        <div
          className="px-5 py-3"
          style={{
            background: "color-mix(in srgb, var(--danger) 8%, transparent)",
            border: "1px solid color-mix(in srgb, var(--danger) 35%, transparent)",
          }}
        >
          <p className="break-words text-sm" style={{ ...sans, color: "var(--danger)" }}>
            <ErrorText error={error} fallback="备份操作失败" showDiagnostic />
          </p>
        </div>
      ) : null}

      {lastBackup ? (
        <Card inset>
          <Eyebrow>最近一次备份</Eyebrow>
          <div className="mt-4 space-y-2 text-sm" style={{ ...sans, color: "var(--fg)" }}>
            <Row label="ID" value={lastBackup.id} />
            <Row label="路径" value={<code style={{ fontFamily: "var(--font-mono)" }}>{lastBackup.path}</code>} />
            <Row label="大小" value={formatBytes(lastBackup.size)} />
            <Row label="SHA256" value={<code style={{ fontFamily: "var(--font-mono)" }}>{lastBackup.sha256.slice(0, 16)}…</code>} />
            <Row label="创建时间" value={new Date(lastBackup.created_at).toLocaleString()} />
          </div>
        </Card>
      ) : (
        <Card inset>
          <div className="text-center py-8" style={{ ...sans, color: "var(--muted)" }}>
            点击"立即备份"创建一份归档。备份存放于路由器本地。
          </div>
        </Card>
      )}

      <RestoreModal
        open={restoreModal}
        onClose={() => setRestoreModal(false)}
        onRestore={async (path) => {
          setError(null);
          try {
            await restore.mutateAsync({ path });
            setRestoreModal(false);
          } catch (e) {
            setError(e);
          }
        }}
        loading={restore.isPending}
        lastPath={lastBackup?.path}
      />
    </div>
  );
}

function Row({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-4">
      <span style={{ color: "var(--muted)" }}>{label}</span>
      <span style={{ color: "var(--fg-strong)" }}>{value}</span>
    </div>
  );
}

function formatBytes(b: number): string {
  if (b < 1024) return `${b} B`;
  if (b < 1024 * 1024) return `${(b / 1024).toFixed(1)} KB`;
  if (b < 1024 * 1024 * 1024) return `${(b / (1024 * 1024)).toFixed(1)} MB`;
  return `${(b / (1024 * 1024 * 1024)).toFixed(2)} GB`;
}

function RestoreModal({
  open,
  onClose,
  onRestore,
  loading,
  lastPath,
}: {
  open: boolean;
  onClose: () => void;
  onRestore: (path: string) => void;
  loading: boolean;
  lastPath?: string;
}) {
  const [path, setPath] = useState(lastPath || "");
  const [error, setError] = useState("");

  function submit() {
    if (!path.trim()) {
      setError("请输入备份文件路径");
      return;
    }
    setError("");
    onRestore(path.trim());
  }

  return (
    <Modal
      open={open}
      onClose={onClose}
      eyebrow="恢复备份"
      title="从归档文件恢复配置"
      width={640}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>取消</Button>
          <Button variant="danger" onClick={submit} loading={loading}>确认恢复</Button>
        </>
      }
    >
      <div className="space-y-5">
        <p className="text-sm leading-7" style={{ ...sans, color: "var(--fg)" }}>
          恢复操作会覆盖当前所有配置（spec、节点、池），并通过 commit lane 落地。该操作不可逆。
        </p>
        <Field label="备份文件路径" hint="路由器上的绝对路径" error={error}>
          <Input
            value={path}
            onChange={(e) => setPath(e.target.value)}
            placeholder="/etc/smartgate/backups/manual-xxxx.tar.gz"
          />
        </Field>
      </div>
    </Modal>
  );
}
