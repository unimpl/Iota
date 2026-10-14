# Chat surface

Mode: Operate.

The primary workflow is select or create a session, confirm its working directory, open it exclusively, submit work, observe progress, answer questions, stop when needed, and release the session for CLI use. Viewing history does not acquire ownership. Refreshing restores conversation output and pending questions from the server. Display execution detail progressively rather than presenting raw event logs as the default conversation.

History initially loads the latest three runs while a quiet tick index lists every run. Hovering or focusing a tick reveals its title and reply summary; clicking loads any missing history and jumps to that run. Scrolling upward loads earlier runs in pages while preserving the current reading position. Buttons fill unloaded gaps between runs and provide an explicit way to load earlier history. Loading failures remain visible with a retry action. These controls follow the existing neutral ChatGPT-style interface on desktop and mobile.
