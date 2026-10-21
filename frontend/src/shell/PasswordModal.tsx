import { useState, useEffect } from "react";
import { usePasswordChange } from "../lib/queries";
import { Modal } from "../ui/Modal";
import { Button } from "../ui/Button";
import { Field, Input } from "../ui/Field";
import { sans } from "../ui/tokens";
import { ErrorText } from "../ui/ErrorText";

export function PasswordModal({ open, onClose }: { open: boolean; onClose: () => void }) {
  const change = usePasswordChange();
  const [oldPwd, setOldPwd] = useState("");
  const [newPwd, setNewPwd] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState<unknown>(null);
  const [done, setDone] = useState(false);

  useEffect(() => {
    if (!open) {
      setOldPwd("");
      setNewPwd("");
      setConfirm("");
      setError(null);
      setDone(false);
    }
  }, [open]);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setError(null);
    if (!oldPwd) {
      setError("请输入当前密码");
      return;
    }
    if (newPwd.length < 6) {
      setError("新密码至少 6 位");
      return;
    }
    if (newPwd !== confirm) {
      setError("两次输入不一致");
      return;
    }
    try {
      await change.mutateAsync({ oldPassword: oldPwd, newPassword: newPwd });
      setDone(true);
    } catch (e) {
      setError(e);
    }
  }

  if (done) {
    return (
      <Modal
        open={open}
        onClose={onClose}
        eyebrow="安全"
        title="密码已修改"
        footer={<Button onClick={onClose}>完成</Button>}
      >
        <p className="text-sm" style={{ ...sans, color: "var(--fg)" }}>
          密码已更新，下次登录请使用新密码。
        </p>
      </Modal>
    );
  }

  return (
    <Modal
      open={open}
      onClose={onClose}
      eyebrow="安全"
      title="修改管理员密码"
      width={480}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>取消</Button>
          <Button onClick={submit as unknown as () => void} loading={change.isPending}>确认修改</Button>
        </>
      }
    >
      <form onSubmit={submit} className="space-y-5">
        <Field label="当前密码" error="">
          <Input
            type="password"
            value={oldPwd}
            onChange={(e) => setOldPwd(e.target.value)}
            autoFocus
          />
        </Field>
        <Field label="新密码" hint="至少 6 位">
          <Input
            type="password"
            value={newPwd}
            onChange={(e) => setNewPwd(e.target.value)}
          />
        </Field>
        <Field
          label="确认新密码"
          error={error ? <ErrorText error={error} fallback="修改密码失败" showDiagnostic /> : undefined}
        >
          <Input
            type="password"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
          />
        </Field>
      </form>
    </Modal>
  );
}
