# Vendored modules

The admin UI's only third-party code, copied unchanged from the npm registry so the UI needs no build step and no package manager.

| File | Package | Path in the package | Tarball sha512 |
|------|---------|---------------------|----------------|
| `preact.mjs` | preact 11.2.0 | `dist/preact.mjs` | `/HfHI/7N84e6GrWBJ3J280z3mLaRJiDcSvj/A+y/4AbLnND/1UlGHe1sR6oYuTGwId0nP7dqDU9OnLxMPZ7xBg==` |
| `hooks.mjs` | preact 11.2.0 | `hooks/dist/hooks.mjs` | (the same tarball) |
| `htm.mjs` | htm 3.1.1 | `dist/htm.mjs` | `983Vyg8NwUE7JkZ6NmOqpCZ+sh1bKv2iYTlUkzlWmA5JD2acKoxd4KVxbMmxX/85mtfdnDmTFoNKcg5DGAvxNQ==` |

`hooks.mjs` imports `preact` by its bare name, which the import map in `index.html` resolves to `preact.mjs`.
Each package's license is beside it, as `LICENSE-preact` and `LICENSE-htm`.

To update one, download its tarball from `https://registry.npmjs.org/<package>/-/<package>-<version>.tgz`, check its sha512 against the registry's `dist.integrity`, copy the files above out of it, and update this table.
