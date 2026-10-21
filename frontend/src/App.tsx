import { useEffect, useState } from "react";
import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApiError } from "./lib/api";
import { AuthGate } from "./auth/AuthGate";
import { Shell } from "./shell/Shell";
import { PageBoundary } from "./shell/PageBoundary";
import { OverviewPage } from "./pages/Overview";
import { ProvidersPage } from "./pages/Providers";
import { NodesPage } from "./pages/Nodes";
import { PoolsPage } from "./pages/Pools";
import { ProxyGroupsPage } from "./pages/ProxyGroups";
import { DnsPage } from "./pages/Dns";
import { RoutesPage } from "./pages/Routes";
import { ConnectionsPage } from "./pages/Connections";
import { SystemPage } from "./pages/System";
import { WifiPage } from "./pages/Wifi";
import { SnapshotsPage } from "./pages/Snapshots";
import { BackupsPage } from "./pages/Backups";
import { SchedulerPage } from "./pages/Scheduler";
import { RuleSetsPage } from "./pages/RuleSets";
import { WorkersPage } from "./pages/Workers";

type ThemeMode = "system" | "light" | "dark";

function useSystemDark() {
  const [dark, setDark] = useState(
    () => typeof window !== "undefined" && window.matchMedia("(prefers-color-scheme: dark)").matches,
  );
  useEffect(() => {
    const m = window.matchMedia("(prefers-color-scheme: dark)");
    const fn = (e: MediaQueryListEvent) => setDark(e.matches);
    m.addEventListener("change", fn);
    return () => m.removeEventListener("change", fn);
  }, []);
  return dark;
}

function useThemeMode() {
  const [mode, setMode] = useState<ThemeMode>(() => {
    const saved = localStorage.getItem("smartgate_theme");
    if (saved === "light" || saved === "dark" || saved === "system") return saved;
    return "system";
  });
  useEffect(() => {
    localStorage.setItem("smartgate_theme", mode);
  }, [mode]);
  const systemDark = useSystemDark();
  const dark = mode === "system" ? systemDark : mode === "dark";
  useEffect(() => {
    const root = document.documentElement;
    root.setAttribute("data-style", "reading");
    root.setAttribute("data-theme", dark ? "dark" : "light");
  }, [dark]);
  return { mode, setMode, dark };
}

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: (count, err) => {
        // 401 / 423 / 409 不再自动重试
        if (err instanceof ApiError) {
          if (err.code === "unauthorized" || err.code === "locked" || err.code === "conflict") {
            return false;
          }
        }
        return count < 2;
      },
      refetchOnWindowFocus: false,
    },
    mutations: { retry: false },
  },
});

export default function App() {
  const { mode, setMode, dark } = useThemeMode();
  return (
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <AuthGate>
          <Shell themeMode={mode} setThemeMode={setMode} dark={dark}>
            <PageBoundary>
              <Routes>
                <Route path="/" element={<Navigate to="/overview" replace />} />
                <Route path="/overview" element={<OverviewPage />} />
                <Route path="/providers" element={<ProvidersPage />} />
                <Route path="/nodes" element={<NodesPage />} />
                <Route path="/workers" element={<WorkersPage />} />
                <Route path="/pools" element={<PoolsPage />} />
                <Route path="/proxy-groups" element={<ProxyGroupsPage />} />
                <Route path="/dns" element={<DnsPage />} />
                <Route path="/rules" element={<RoutesPage />} />
                <Route path="/rule-sets" element={<RuleSetsPage />} />
                <Route path="/connections" element={<ConnectionsPage />} />
                <Route path="/system" element={<SystemPage />} />
                <Route path="/wifi" element={<WifiPage />} />
                <Route path="/snapshots" element={<SnapshotsPage />} />
                <Route path="/backups" element={<BackupsPage />} />
                <Route path="/scheduler" element={<SchedulerPage />} />
                <Route path="*" element={<Navigate to="/overview" replace />} />
              </Routes>
            </PageBoundary>
          </Shell>
        </AuthGate>
      </BrowserRouter>
    </QueryClientProvider>
  );
}
