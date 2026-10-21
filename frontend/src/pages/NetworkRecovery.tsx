import { useEffect, useState } from "react";
import { api } from "../lib/api";
import { Card, SectionHeader } from "../ui/Card";
import { Badge } from "../ui/Badge";
import { Button } from "../ui/Button";
import { Modal } from "../ui/Modal";

type Report = {
  status: "ok" | "degraded" | "disabled" | "unavailable";
  checked_at: string;
  checks: { id: string; label: string; ok: boolean; detail: string }[];
};

export function NetworkRecovery() {
  const [report, setReport] = useState<Report | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [confirm, setConfirm] = useState(false);
  useEffect(() => {
    const controller = new AbortController();
    setBusy(true);
    api.get<Report>("/system/network/diagnostics", { signal: controller.signal })
      .then(setReport).catch((e) => { if (!controller.signal.aborted) setError(String(e)); })
      .finally(() => { if (!controller.signal.aborted) setBusy(false); });
    return () => controller.abort();
  }, []);
  async function run(repair: boolean) {
    setConfirm(false); setBusy(true); setError(""); setReport(null);
    try {
      setReport(repair
        ? await api.post<Report>("/system/network/repair", { confirm: true })
        : await api.get<Report>("/system/network/diagnostics"));
    } catch (e) { setError(e instanceof Error ? e.message : "网络检查失败"); }
    finally { setBusy(false); }
  }
  return <section className="space-y-5">
    <SectionHeader eyebrow="网络恢复" title="网络诊断与修复" desc="检查 DNS 接管及代理外网访问。重启 SmartGate 时也会自动恢复已应用配置对应的网络服务。" />
    <Card inset>
      <div className="flex flex-wrap gap-3 items-center">
        <Button variant="ghost" loading={busy} disabled={busy} onClick={() => run(false)}>检查网络</Button>
        <Button disabled={busy || report?.status === "disabled"} onClick={() => setConfirm(true)}>恢复网络服务</Button>
        {report && <Badge tone={report.status === "ok" ? "ok" : "warn"}>
          {report.status === "ok" ? "路由器检查通过" : report.status === "disabled" ? "直连或救援模式，未开启代理" : report.status === "unavailable" ? "无法读取网络状态" : "发现异常"}
        </Badge>}
      </div>
      {error && <p role="alert" className="mt-4 text-sm" style={{ color: "var(--danger)" }}>{error}</p>}
      {report && <ul className="mt-4 space-y-3 text-sm">
        {report.checks.map(c => <li key={c.id} className="flex justify-between gap-4"><span>{c.label}</span><span title={c.detail} style={{ color: c.ok ? "var(--fg)" : "var(--danger)" }}>{c.ok ? "通过" : "失败"}</span></li>)}
      </ul>}
      <p className="mt-4 text-xs" style={{ color: "var(--muted)" }}>恢复会短暂中断代理连接，保留已应用的规则和选点，不应用尚未生效的修改。检查通过后仍需在手机或电脑上确认访问；上游线路故障无法保证通过重启恢复。</p>
    </Card>
    <Modal open={confirm} onClose={() => setConfirm(false)} eyebrow="网络恢复" title="恢复网络服务？" footer={<><Button variant="ghost" onClick={() => setConfirm(false)}>取消</Button><Button onClick={() => run(true)}>确认恢复</Button></>}>
      <p className="text-sm leading-7">将恢复代理服务、DNS 接管与防火墙自动加载，并重新检查网络。期间连接会短暂中断，无需重启路由器。</p>
    </Modal>
  </section>;
}
