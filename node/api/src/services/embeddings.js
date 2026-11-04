const { apiBase } = require('./api-base');

const MODEL = 'text-embedding-3-small';

async function embed(texts) {
    const input = Array.isArray(texts) ? texts : [texts];

    const response = await fetch(apiBase('OPENAI_API_BASE') + '/v1/embeddings', {
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
