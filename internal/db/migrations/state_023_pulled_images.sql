-- Public images ah pulled for image-sourced sandboxes. A ref is recorded only
-- when the image was absent locally before the sandbox create, so images the
-- operator pulled (Traefik, registry, database engines) are never recorded.
-- GC removes a recorded image once no container uses it and no service,
-- snapshot, environment or template row references it.
CREATE TABLE IF NOT EXISTS pulled_images (
  ref TEXT PRIMARY KEY,
  pulled_at INTEGER NOT NULL
);
