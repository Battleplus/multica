-- Record which issue a cancelled issue duplicates (MUL-7349).
--
-- A duplicate is an ordinary cancelled issue with a pointer to the issue it
-- duplicates; there is no duplicate status. The pointer only lives while the
-- issue is cancelled: UpdateIssue and UpdateIssueStatus clear it whenever the
-- status moves away from cancelled, which is how a duplicate mark is removed.
--
-- No foreign key (repository rule). Deleting the original clears the pointers
-- to it in the same transaction, the way deleting a parent detaches children.
--
-- A nullable column with no default is a catalog-only change, so this does not
-- rewrite the table. Bound lock acquisition so the ALTER fails fast and retries
-- on the next run instead of queueing an ACCESS EXCLUSIVE lock in front of
-- every issue query.
SET LOCAL lock_timeout = '2s';
SET LOCAL statement_timeout = '10s';

ALTER TABLE issue ADD COLUMN IF NOT EXISTS duplicate_of_issue_id UUID;
