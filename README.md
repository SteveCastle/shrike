## Shrike Media Server

**Shrike Media Server is an the alpha phase of development. Only use it if you know what you're doing.**

Shrike Media server is a companion for the Lowkey Media Viewer. It allows for managing long running offline tasks like, media tagging, transcript generation, media conversion, file serving, and media ingestion.

## Installation
1. Run the shrike.exe binary. This will start the background server and create an Icon in your Windows system tray used for launching the WebUI.
<img width="408" height="247" alt="Screenshot 2025-09-20 080659" src="https://github.com/user-attachments/assets/4b8a0141-08d4-4fb9-9c42-78db5dbd25ad" />

2. Check Lowkey Database Path. Open the Config tab in the Web UI. You should see the path to your Lowkey Media Database. If it's incorrect you can manually change it here.
3. Set Up model paths. You'll need to point the server at a few files to enable auto tagging.
  * [Download Model Files for AutoTagger](https://huggingface.co/SmilingWolf/wd-eva02-large-tagger-v3/tree/main)
  * [Install Ollama for LLM based description generation.](https://ollama.com/)
  * [Download Faster Whisper for Video Transcription generation.](https://github.com/Purfview/whisper-standalone-win)
<img width="1233" height="1693" alt="Screenshot 2025-09-20 080416" src="https://github.com/user-attachments/assets/5eb008ae-88fb-4519-af03-4e55afbb6601" />


## Usage
Once the server is installed. Lowkey Media Viewer should detect it and be able to create jobs. You can also create bulk jobs on the results of any search from the Media Browser tab.
