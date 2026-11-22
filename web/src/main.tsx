import "./styles.css";
import { render } from "@solidjs/web";
import { Loading } from "solid-js";
import { PreferencesProvider } from "./preferences";
import { Router } from "./router";

render(
  () => (
    <PreferencesProvider>
      <Router>{(props) => <Loading>{props.children}</Loading>}</Router>
    </PreferencesProvider>
  ),
  document.getElementById("root")!,
);
