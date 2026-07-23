import React from "react";
import ReactDOM from "react-dom/client";
import App from "./App";
import PanelApp from "./PanelApp";
import "./styles.css";

// トレイのポップオーバーは同じビルド成果物を `?panel=1` クエリ付きで開き、別モード(PanelApp)
// として描画する(デスクトップトレイ設計 2026-07-23 §3.2)。
const isPanel = new URLSearchParams(window.location.search).get("panel") === "1";

ReactDOM.createRoot(document.getElementById("root") as HTMLElement).render(
  <React.StrictMode>
    {isPanel ? <PanelApp /> : <App />}
  </React.StrictMode>,
);
