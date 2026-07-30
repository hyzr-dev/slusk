import { describe, expect, it } from 'vitest';
import { jobsPageUrl, queryKeys } from './queries';
import type { JobPageParams } from './types';

const base: JobPageParams = {
  page: 0,
  sort: 'st',
  dir: 'asc',
  filter: 'all',
  source: 'all',
  q: '',
};

describe('jobsPageUrl', () => {
  it('omits facets unless skipFacets is set', () => {
    expect(jobsPageUrl(base)).not.toContain('facets');
  });

  it('sends facets=0 when skipFacets is set', () => {
    const url = jobsPageUrl({ ...base, filter: 'finished', sort: 'recent', dir: 'desc', pageSize: 5, skipFacets: true });
    expect(url).toContain('facets=0');
    expect(url).toContain('filter=finished');
    expect(url).toContain('sort=recent');
    expect(url).toContain('dir=desc');
  });

  it('keys the cache on skipFacets so two otherwise identical queries do not collide', () => {
    const withFacets = queryKeys.jobsPage(base);
    const without = queryKeys.jobsPage({ ...base, skipFacets: true });
    expect(withFacets).not.toEqual(without);
  });
});
