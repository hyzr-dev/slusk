-- Issue #508: a peer with a ruined download history is still ranked first
-- whenever it advertises the best-looking files, because matcher.Rank is
-- purely additive and no weight can express "last of all".
--
-- The ranker now sorts such candidates into a separate, always-later tier
-- (matcher.IsLastResortPeer). This column records that a candidate was picked
-- out of that tier, so the job detail view can say why an obviously bad peer
-- was chosen anyway - without it, the display is indistinguishable from the
-- bug #507 describes.
--
-- The flag is written once, at insert, and never recomputed. It must answer a
-- historical question ("was this peer last resort when we picked it?"), and
-- recomputing it from current peer history at read time would answer a
-- different one - the two disagree exactly when the peer's record has since
-- decayed or worsened, which is the interesting case.
--
-- Additive and defaulted, so an older instance still running through a rolling
-- deploy keeps inserting candidates without knowing the column exists. Rows
-- written before this migration get false, which is the honest value: they were
-- ranked by a matcher that had no tier.
ALTER TABLE candidates
    ADD COLUMN IF NOT EXISTS last_resort BOOLEAN NOT NULL DEFAULT FALSE;

-- No index. The flag is only ever read back alongside a candidate row already
-- located by album_job_id or id; nothing filters or orders by it.
