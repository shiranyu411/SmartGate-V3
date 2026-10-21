import { Component, type ReactNode } from "react";
import { Link, useLocation } from "react-router-dom";
import { Button } from "../ui/Button";
import { sans } from "../ui/tokens";

export function PageBoundary({ children }: { children: ReactNode }) {
  const location = useLocation();
  return <PageErrorBoundary key={location.pathname}>{children}</PageErrorBoundary>;
}

class PageErrorBoundary extends Component<{ children: ReactNode }, { failed: boolean }> {
  state = { failed: false };

  static getDerivedStateFromError() {
    return { failed: true };
  }

  render() {
    if (!this.state.failed) return this.props.children;
    return (
      <section role="alert" className="space-y-4 max-w-xl" style={sans}>
        <h2 className="text-xl font-medium" style={{ color: "var(--fg-strong)" }}>
          页面暂时无法显示
        </h2>
        <div className="flex flex-wrap items-center gap-4">
          <Button onClick={() => this.setState({ failed: false })}>重试</Button>
          <Link to="/overview" style={{ color: "var(--accent)" }}>返回概览</Link>
        </div>
      </section>
    );
  }
}
