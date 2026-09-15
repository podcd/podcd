# Website

This website is built using [Docusaurus](https://docusaurus.io/), a modern static website generator. Its structure follows oauth2-proxy's docs; its look follows helm.sh.

## Installation

```console
npm install
```

## Local development

```console
npm start
```

This command starts a local development server and opens a browser window. Most changes are reflected live without having to restart the server.

## Build

```console
npm run build
```

This command generates static content into the `build` directory, which can be served by any static content host. This is what CI runs on every pull request touching `docs/`, and what it deploys to GitHub Pages on every push to `main` (see `.github/workflows/docs.yml`).

## Versions

`docs/` tracks `main`, shown as **main** in the version dropdown. To freeze the docs for a release:

```console
npm run docusaurus docs:version 2.3
```

This copies `docs/` into `versioned_docs/version-2.3/` and lists `2.3` in `versions.json`; the dropdown then offers both. Only cut a version when the docs for that release are final.
