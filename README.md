# MezzaOps

MezzaOps is a Discord bot that manages other processes. The intent is to use it as a meta-bot for running other bots.

Usage:
```sh
go install .
echo $BOT_TOKEN > token.txt
mezzaops --guild_id=$SERVER_ID --channel-id=$CHANNEL_ID
```

The processes managed by the server are configured in `tasks.yaml`.

Commands currently available (followed by a task name):
* `/ops start`
* `/ops stop`
* `/ops restart`
* `/ops logs`
* `/ops status`
* `/ops pull` - does `git pull` in the process directory. The git repo needs to have been set up for this to run smoothly (upstream set etc.). More git commands are coming.

This is currently (as of 2023-02-26) the result of about a day's hacking, it's usable but very barebones and not robust. More to come.
