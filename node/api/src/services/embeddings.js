const OPENAI_URL = 'https://api.openai.com/v1/embeddings';
const MODEL = 'text-embedding-3-small';

async function embed(texts) {
    const input = Array.isArray(texts) ? texts : [texts];

    const response = await fetch(OPENAI_URL, {
        method: 'POST',
        headers: {
            'Content-Type': 'application/json',
            'Authorization': `Bearer ${process.env.OPENAI_API_KEY}`
        },
        body: JSON.stringify({ model: MODEL, input })
    });

    if (!response.ok) {
        const body = await response.text();
        throw new Error(`OpenAI API error ${response.status}: ${body}`);
    }

    const data = await response.json();
    return data.data.map(item => item.embedding);
}

module.exports = { embed };
