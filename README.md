# unitmgr

Manage the state of systemd units without touching systemctl.


## Usage

```bash
# start unitmgr (in the real world this should be systemd service instead)
unitmgr -src /units &

# copy over a unit file to start running it
# any changes to the file will cause the unit to be restarted
cp myprocess.service /units

# delete the unit to stop running it
rm /units/myprocess.service

# that's all!
```
