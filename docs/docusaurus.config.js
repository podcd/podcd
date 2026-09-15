// @ts-check
// See: https://docusaurus.io/docs/api/docusaurus-config

import { themes as prismThemes } from 'prism-react-renderer';

/** @type {import('@docusaurus/types').Config} */
const config = {
  title: 'podcd',
  tagline: 'A Git-driven reconciler for Linux workloads.',
  favicon: 'img/icon.png',

  // v4 turns on the Rspack-based "Faster" build, hence @docusaurus/faster in package.json.
  future: {
    v4: true,
    faster: true,
  },

  // GitHub Pages: https://podcd.github.io/podcd/
  url: 'https://podcd.github.io',
  baseUrl: '/podcd/',
  organizationName: 'podcd',
  projectName: 'podcd',

  onBrokenLinks: 'throw',
  onBrokenAnchors: 'throw',

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  presets: [
    [
      'classic',
      /** @type {import('@docusaurus/preset-classic').Options} */
      ({
        docs: {
          sidebarPath: require.resolve('./sidebars.js'),
          // The docs are the site: no landing page in front of them.
          routeBasePath: '/',
          editUrl: 'https://github.com/podcd/podcd/edit/main/docs/',
          // docs/ tracks main. A release cuts a snapshot with
          // `npm run docusaurus docs:version <x.y>`, which lands in
          // versioned_docs/ and shows up in the version dropdown.
          lastVersion: 'current',
          versions: {
            current: { label: 'main' },
          },
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      }),
    ],
  ],

  themes: [
    '@docusaurus/theme-mermaid',
    [
      require.resolve('@easyops-cn/docusaurus-search-local'),
      /** @type {import("@easyops-cn/docusaurus-search-local").PluginOptions} */
      ({
        hashed: true,
        docsDir: 'docs',
        language: ['en'],
        indexDocs: true,
        indexBlog: false,
        indexPages: false,
        docsRouteBasePath: '/',
        searchResultLimits: 10,
        searchBarShortcut: true,
        removeDefaultStemmer: true,
        searchBarShortcutHint: true,
        highlightSearchTermsOnTargetPage: true,
      }),
    ],
  ],

  markdown: {
    mermaid: true,
    hooks: {
      onBrokenMarkdownLinks: 'throw',
    },
  },

  themeConfig:
    /** @type {import('@docusaurus/preset-classic').ThemeConfig} */
    ({
      image: 'img/logo-horizontal.svg',
      colorMode: {
        defaultMode: 'dark',
        disableSwitch: false,
        respectPrefersColorScheme: false,
      },
      docs: {
        sidebar: {
          hideable: true,
        },
      },
      navbar: {
        title: 'podcd',
        logo: {
          alt: 'podcd',
          src: 'img/logo-horizontal.svg',
          srcDark: 'img/logo-horizontal-dark.svg',
        },
        items: [
          {
            type: 'docSidebar',
            sidebarId: 'docs',
            position: 'left',
            label: 'Docs',
          },
          {
            type: 'docsVersionDropdown',
            position: 'right',
            dropdownActiveClassDisabled: true,
          },
          {
            href: 'https://github.com/podcd/podcd-gitops',
            label: 'Examples',
            position: 'right',
          },
          {
            href: 'https://github.com/podcd/podcd',
            label: 'GitHub',
            position: 'right',
          },
        ],
      },
      footer: {
        style: 'dark',
        links: [
          {
            title: 'Project',
            items: [
              { label: 'Source code', href: 'https://github.com/podcd/podcd' },
              { label: 'Releases', href: 'https://github.com/podcd/podcd/releases' },
              { label: 'Issues', href: 'https://github.com/podcd/podcd/issues' },
            ],
          },
          {
            title: 'Examples',
            items: [
              { label: 'podcd-gitops', href: 'https://github.com/podcd/podcd-gitops' },
              { label: 'multi-env', href: 'https://github.com/podcd/podcd-gitops/tree/main/multi-env' },
            ],
          },
          {
            title: 'Community',
            items: [
              { label: 'Contributing', to: 'community/contribution' },
              { label: 'Security', to: 'community/security' },
            ],
          },
        ],
        copyright: `© ${new Date().getFullYear()} podcd authors. MIT License.`,
      },
      prism: {
        theme: prismThemes.github,
        darkTheme: prismThemes.dracula,
        additionalLanguages: ['bash', 'ini', 'diff'],
      },
    }),
};

export default config;
