import { JsonParser } from "@streamparser/json";

async function parseJsonObjectStream(stream, handler) {
  const parser = new JsonParser({ paths: ["$"] });
  parser.onValue = handler;

  const reader = stream.getReader();
  const parse = async () => {
    const { done, value } = await reader.read();
    if (done) {
      return;
    }
    try {
      parser.write(value);
    } catch (e) {
      console.error("failed to parse json", e);
      return;
    }
    await parse();
  };
  await parse();
}

let pc = new RTCPeerConnection({
  iceServers: [
    {
      urls: "stun:stun.l.google.com:19302",
    },
  ],
});

const logContainer = document.getElementById("logs");
let log = (msg) => {
  console.log(msg);
  logContainer.innerHTML += msg + "<br>";
};

const player = document.getElementById("player");
pc.ontrack = function (event) {
  const track = event.streams[0];
  console.debug("received track", { event, track });
  player.srcObject = track;
};

pc.oniceconnectionstatechange = (e) => log(pc.iceConnectionState);
let sessionDescription = null;
pc.onicecandidate = (event) => {
  console.debug("got ice candidate", { candidate: event.candidate });
  if (event.candidate === null) {
    console.debug("ice candidate succeeded");
    sessionDescription = pc.localDescription;
  }
};

// Offer to receive 1 audio
pc.addTransceiver("audio", {
  direction: "sendrecv",
});

pc.createOffer()
  .then((d) => {
    console.debug("setting local description", d);
    pc.setLocalDescription(d);
  })
  .catch(log);

const startSession = async () => {
  const response = await fetch("https://marcus.stromaproxy.kalk.space/sdp", {
    method: "POST",
    headers: {
      "content-type": "application/json",
    },
    body: JSON.stringify(sessionDescription),
  });

  if (!response.ok) {
    log(`http request failed: ${response.statusCode}`);
    return;
  }

  await parseJsonObjectStream(response.body, (value) => {
    console.debug("got json:", value);
    const { error } = value;
    if (error) {
      console.error("got error from backend:", value.error);
      return;
    }
    if ("sdp" in value) {
      // treat as remote description
      pc.setRemoteDescription(new RTCSessionDescription(value));
      console.log("set remote description");
      return;
    }
    if ("candidate" in value) {
      pc.addIceCandidate(value);
      return;
    }
    console.warn("received unexpected message", value);
  });
  console.debug("parsed all json from body");
};

document.getElementById("startButton").addEventListener("click", startSession);
