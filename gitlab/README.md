# gitlab

Your GitLab todos as a grid: one well per week, one text tile per todo.
Trashing a todo marks it done at GitLab.

## Config

| Key | Default | Meaning |
|---|---|---|
| `token_file` | required | A file holding a personal access token: `read_api` to read, `api` to mark done. |
| `url` | `https://gitlab.com` | The GitLab instance. |
| `refresh` | `30s` | While a todo grid is shown, how often the plugin reads the newest page of your pending todos. A new todo shows within this. |
| `full_refresh` | `10m` | How long a read of every page stays fresh. A todo done or deleted at GitLab shows done within this while a todo grid is shown, or the next time you open one after it has passed. |

GitLab does not push changes to a user's todos, so the plugin polls, and
only while one of its grids is shown: the node holds the plugin's change
stream open exactly then, and the polling stops ten seconds after it closes.
The frequent read is one request; only the full read can tell that a todo
has left the pending list. When either read changes what the plugin knows,
it tells the node which grids changed, and every view showing them repaints.

The plugin remembers every todo it has read, in its state directory, so a
restart shows them at once. When GitLab cannot be read, the grids keep
showing what the plugin last read and the plugin's status says why, until a
read works again.
