// @ts-check

/** @type {import('@docusaurus/plugin-content-docs').SidebarsConfig} */
const sidebars = {
  docs: [
    {
      type: 'doc',
      id: 'welcome',
    },
    {
      type: 'doc',
      id: 'installation',
    },
    {
      type: 'doc',
      id: 'overview',
    },
    {
      type: 'category',
      label: 'Configuration',
      link: {
        type: 'doc',
        id: 'configuration/overview',
      },
      collapsed: false,
      items: [
        'configuration/agent',
        'configuration/model',
        'configuration/values',
        'configuration/secrets',
      ],
    },
    {
      type: 'category',
      label: 'Reference',
      link: {
        type: 'doc',
        id: 'reference/cli',
      },
      collapsed: false,
      items: ['reference/cli'],
    },
    {
      type: 'category',
      label: 'Community',
      link: {
        type: 'doc',
        id: 'community/contribution',
      },
      collapsed: false,
      items: ['community/contribution', 'community/security'],
    },
  ],
};

export default sidebars;
