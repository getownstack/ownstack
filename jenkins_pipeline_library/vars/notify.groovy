// Sends a pipeline notification through the stack's alerting aspect.
// ownstackctl sets OWNSTACK_SLACK_CHANNEL when alerts use Slack; without an
// alerting provider this is a no-op.
def call(String color, String message) {
    if (!env.OWNSTACK_SLACK_CHANNEL) {
        return
    }
    slackSend(channel: env.OWNSTACK_SLACK_CHANNEL, color: color, message: message)
}
