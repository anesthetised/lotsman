import "./styles.css";
import { render } from "@solidjs/web";
import { Loading } from "solid-js";
import { i18n, I18nContext, pickLocale } from "./i18n";
import { Router } from "./router";

const lang = i18n(pickLocale(navigator.languages));
document.documentElement.lang = lang.locale;
document.title = lang.t.app.name;

render(
  () => (
    <I18nContext value={lang}>
      <Router>{(props) => <Loading>{props.children}</Loading>}</Router>
    </I18nContext>
  ),
  document.getElementById("root")!,
);
