# gitlab

Your GitLab todos as a grid: one well per week, one text tile per todo.
Trashing a todo marks it done at GitLab.

## Config

| Key | Default | Meaning |
|---|---|---|
| `token_file` | required | A file holding a personal access token: `read_api` to read, `api` to mark done. |
| `url` | `https://gitlab.com` | The GitLab instance. |
| `refresh` | `30s` | How often the plugin reads the newest page of your pending todos. A new todo shows within this. |
| `full_refresh` | `10m` | How often the plugin reads every page. A todo done or deleted at GitLab shows done within this. |

GitLab does not push changes to a user's todos, so the plugin polls. The
frequent read is one request; only the full read can tell that a todo has
left the pending list. When either read changes what the plugin knows, it
tells the node which grids to repaint.
