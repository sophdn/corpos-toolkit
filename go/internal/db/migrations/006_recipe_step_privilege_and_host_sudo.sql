-- Recipe-step privilege declaration + host passwordless-sudo capability.
--
-- Bug #1017 surfaced: recipe steps that need root composed `sudo` into
-- the command body itself. On hosts where the ssh_user lacks NOPASSWD
-- sudo (the homelab default), every such step failed mid-walk with the
-- canonical 'sudo: a terminal is required to read the password' error
-- under SystemSsh's BatchMode=yes. The recipe gave no signal at forge
-- time that this risk existed; the user discovered it at apply time.
--
-- Schema response:
--   - recipe_steps.privilege ∈ {'user', 'sudo', 'root'}; default 'user'.
--     'sudo' wraps the command in `sudo -n` at apply time. 'root'
--     requires the host's ssh_user IS already root.
--   - hosts.passwordless_sudo (bool, 0 default). apply_recipe pre-
--     flights every sudo-marked step against this capability and
--     halts before walking if any step would fail.

ALTER TABLE recipe_steps ADD COLUMN privilege TEXT NOT NULL DEFAULT 'user';

ALTER TABLE hosts ADD COLUMN passwordless_sudo INTEGER NOT NULL DEFAULT 0;
