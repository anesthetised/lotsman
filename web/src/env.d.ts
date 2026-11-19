/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** Set (to "1") to serve the in-memory fake daemon instead of the panel API. */
  readonly VITE_FAKE_API?: string;
}
