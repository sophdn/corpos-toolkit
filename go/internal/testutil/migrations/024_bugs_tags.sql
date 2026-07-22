-- Add tags to bugs. Comma-separated or newline-dash-joined list of
-- categorical labels (e.g. 'kiwix,forge' or '\n- kiwix\n- forge').
-- Default empty string: existing rows carry no tags.

ALTER TABLE bugs ADD COLUMN tags TEXT NOT NULL DEFAULT '';
