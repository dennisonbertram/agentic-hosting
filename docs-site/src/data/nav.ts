export type NavItem = {
  slug: string;
  title: string;
};

export type NavSection = {
  label: string;
  items: NavItem[];
};

export const nav: NavSection[] = [
  {
    label: 'Start',
    items: [
      { slug: '', title: 'Overview' },
      { slug: 'for-agents', title: 'Give this to your agent' },
      { slug: 'why', title: 'Why it exists' },
      { slug: 'architecture', title: 'Architecture' },
      { slug: 'install', title: 'Install & quickstart' },
    ],
  },
  {
    label: 'Use',
    items: [
      { slug: 'auth', title: 'Registration & API keys' },
      { slug: 'deploy', title: 'Deploy from git' },
      { slug: 'databases', title: 'Postgres & Redis' },
      { slug: 'env', title: 'Env vars & secrets' },
    ],
  },
  {
    label: 'Operate',
    items: [
      { slug: 'reconciliation', title: 'Reconciliation' },
      { slug: 'guards', title: 'Crash-loop & disk guards' },
      { slug: 'rate-limits', title: 'Rate limits' },
      { slug: 'backups', title: 'Backups' },
    ],
  },
  {
    label: 'Reference',
    items: [
      { slug: 'api', title: 'REST API' },
      { slug: 'claude-code', title: 'Claude Code skill' },
      { slug: 'security', title: 'Security model' },
      { slug: 'troubleshooting', title: 'Troubleshooting' },
      { slug: 'roadmap', title: 'Roadmap' },
    ],
  },
];

export const githubRepo = 'https://github.com/dennisonbertram/agentic-hosting';
export const githubStar = githubRepo;
export const siteName = 'agentic.hosting';
export const siteTagline = 'Hosting infrastructure built for AI agents';
