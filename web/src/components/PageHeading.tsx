import type { ReactNode } from 'react';

export default function PageHeading({ children }: { children: ReactNode }) {
  return <h1 style={{ margin: '0 0 12px', fontSize: 18, fontWeight: 600 }}>{children}</h1>;
}
